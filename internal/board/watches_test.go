// internal/board/watches_test.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// TestMigrationFiveAddsTheWatchesTable upgrades a real version-4 board and checks
// the table, the columns M4's decisions add, the due index the scheduler scans
// through, and the trigger vocabulary the schema enforces.
func TestMigrationFiveAddsTheWatchesTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	ctx := context.Background()

	// The fixture is a version-4 database, built from the list rather than Open
	// (M3's lesson): Open would stamp whatever this binary implements, and the
	// "v4 board" under test would silently be a v5 one.
	if err := migrate(ctx, raw, migrations[:4]); err != nil {
		t.Fatalf("migrate to version 4: %v", err)
	}
	if err := migrate(ctx, raw, migrations); err != nil {
		t.Fatalf("migrate to version 5: %v", err)
	}

	var v int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != schemaVersion {
		t.Fatalf("user_version = %d after migrating, want %d (schemaVersion)", v, schemaVersion)
	}

	// Every column a fire, a skip or a condition edge writes. last_result and
	// the two skip columns are M4's additions to the spec's table; losing one is
	// losing D5 or D7, so the test names them.
	for _, want := range []struct {
		name    string
		notNull int
	}{
		{"id", 0}, {"name", 1}, {"trigger", 1}, {"schedule", 0}, {"probe", 0},
		{"template", 1}, {"dedupe_window", 1}, {"cooldown_until", 0}, {"enabled", 1},
		{"last_fired_at", 0}, {"next_fire_at", 0}, {"last_result", 0},
		{"last_skip_at", 0}, {"last_skip_reason", 0}, {"created_at", 1},
	} {
		var name, typ string
		var notNull, pk int
		var dflt sql.NullString
		err := raw.QueryRow(`SELECT name, type, "notnull", COALESCE(dflt_value,''), pk
		                       FROM pragma_table_info('watches') WHERE name = ?`, want.name).
			Scan(&name, &typ, &notNull, &dflt, &pk)
		if err != nil {
			t.Fatalf("watches.%s: %v", want.name, err)
		}
		if notNull != want.notNull {
			t.Fatalf("watches.%s notnull = %d, want %d", want.name, notNull, want.notNull)
		}
	}

	// The due scan is `enabled=1 AND next_fire_at <= ?`, and the index has to be
	// the partial one: a full index would carry every disabled watch forever.
	rows, err := raw.Query(`PRAGMA index_list('watches')`)
	if err != nil {
		t.Fatalf("index_list: %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatalf("scan index_list: %v", err)
		}
		if name == "idx_watches_due" {
			found = true
			if partial != 1 {
				t.Fatalf("idx_watches_due is not partial; disabled watches would occupy it forever")
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index_list rows: %v", err)
	}
	if !found {
		t.Fatal("idx_watches_due is missing; the scheduler scan has no index")
	}

	// The trigger vocabulary is a database constraint, not a Go convention: a
	// fourth trigger must fail at INSERT, because a scheduler that meets one
	// would have to guess what to do with it.
	if _, err := raw.Exec(`INSERT INTO watches (id,name,trigger,schedule,template,created_at)
	                       VALUES ('w1','bad','hourly','x','{}',1)`); err == nil {
		t.Fatal("the schema accepted trigger 'hourly'; the vocabulary is enforced in Go only")
	}
}

// TestAVersionFiveDatabaseRefusesAnOlderBinary hands a real version-5 database to
// the migration list a version-4 binary ships. It must refuse before applying
// anything: a v4 binary that "upgraded" a v5 board would not know about watches
// and would answer "no watches" to every route that reads them.
func TestAVersionFiveDatabaseRefusesAnOlderBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer raw.Close()
	ctx := context.Background()

	if err := migrate(ctx, raw, migrations[:5]); err != nil {
		t.Fatalf("migrate to version 5: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO watches (id,name,trigger,schedule,template,created_at)
	                       VALUES ('w1','nightly','cron','30 2 * * *','{}',1)`); err != nil {
		t.Fatalf("seed watch: %v", err)
	}

	err = migrate(ctx, raw, migrations[:4])
	if err == nil {
		t.Fatal("a version-4 migration list accepted a version-5 database, want a refusal")
	}
	if !strings.Contains(err.Error(), "newer than this binary") || !strings.Contains(err.Error(), "v4") {
		t.Fatalf("err = %q, want a newer-than-this-binary refusal naming v4", err)
	}

	var v int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != 5 {
		t.Fatalf("user_version = %d after the refusal, want 5 (nothing applied)", v)
	}
}

// TestWatchTemplateRoundTrips stores a template and reads it back: the bytes the
// board holds must be the bytes it would write again, an empty need set must
// survive as [] rather than null, and the fields a fire needs must come back
// unchanged.
func TestWatchTemplateRoundTrips(t *testing.T) {
	db, _ := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))

	cases := []struct {
		name string
		tmpl WatchTemplate
	}{
		{"no needs", WatchTemplate{Title: "nightly", Brief: "check the thing"}},
		{"explicitly empty needs", WatchTemplate{Title: "nightly", Brief: "check the thing", Needs: []string{}}},
		{"two needs", WatchTemplate{
			Title: "nightly", Brief: "check the thing", Needs: []string{"agentic", "network"},
			Priority: 5, MaxAttempts: 2,
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.tmpl)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			id := "w" + string(rune('a'+i))
			if _, err := db.Exec(`INSERT INTO watches (id,name,trigger,schedule,template,created_at)
			                      VALUES (?,?,?,?,?,1)`, id, "name-"+id, "interval", "5m", string(encoded)); err != nil {
				t.Fatalf("store: %v", err)
			}
			var stored string
			if err := db.QueryRow(`SELECT template FROM watches WHERE id=?`, id).Scan(&stored); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if stored != string(encoded) {
				t.Fatalf("stored %s, want the encoded bytes %s", stored, encoded)
			}
			var got WatchTemplate
			if err := json.Unmarshal([]byte(stored), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Title != tc.tmpl.Title || got.Brief != tc.tmpl.Brief ||
				got.Priority != tc.tmpl.Priority || got.MaxAttempts != tc.tmpl.MaxAttempts {
				t.Fatalf("round-tripped %+v, want %+v", got, tc.tmpl)
			}
			if !slices.Equal(got.Needs, tc.tmpl.Needs) {
				t.Fatalf("needs = %v, want %v", got.Needs, tc.tmpl.Needs)
			}
			if len(tc.tmpl.Needs) == 0 {
				if got.Needs == nil {
					t.Fatal("empty needs read back as nil; a reader ranging them gets null")
				}
				if !strings.Contains(stored, `"needs":[]`) {
					t.Fatalf("empty needs stored as %s, want \"needs\":[]", stored)
				}
			}
		})
	}
}

// watchReq is a valid interval watch, the base every create test perturbs.
// deref renders an optional timestamp in a failure message. Printing the pointer
// itself gives an address, and a failure message's job is to tell the reader the
// value it found.
func deref(p *int64) string {
	if p == nil {
		return "nil"
	}
	return stamp(*p)
}

func watchReq(name string) CreateWatchRequest {
	return CreateWatchRequest{
		Name: name, Trigger: TriggerInterval, Schedule: "5m",
		Template: WatchTemplate{
			Title: "check the disk", Brief: "run df and report if the volume is over 90%",
			Needs: []string{"agentic"},
		},
	}
}

// TestCreatingAWatchComputesItsFirstFire: a new watch is due at its next slot, not
// immediately, so creating one does not burst a backlog of catch-up items.
func TestCreatingAWatchComputesItsFirstFire(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()

	got, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	if got.NextFireAt == nil || *got.NextFireAt != clock.Now()+300 {
		t.Fatalf("next_fire_at = %s, want %s (five minutes after now)", deref(got.NextFireAt), stamp(clock.Now()+300))
	}
	if !got.Enabled {
		t.Fatal("a new watch is not enabled")
	}

	// Cron: the fixed clock is 2023-11-14T22:13:20Z, so */5 is due at 22:15:00Z.
	req := watchReq("cron")
	req.Trigger, req.Schedule = TriggerCron, "*/5 * * * *"
	cronWatch, err := CreateWatch(ctx, db, clock, req)
	if err != nil {
		t.Fatalf("CreateWatch cron: %v", err)
	}
	if cronWatch.NextFireAt == nil || *cronWatch.NextFireAt != 1_700_000_100 {
		t.Fatalf("cron next_fire_at = %s, want %s (22:15:00Z)", deref(cronWatch.NextFireAt), stamp(1_700_000_100))
	}

	// And the row agrees with the value returned to the caller.
	stored, err := GetWatch(ctx, db, got.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	if stored.NextFireAt == nil || *stored.NextFireAt != *got.NextFireAt {
		t.Fatalf("stored next_fire_at = %s, want %s", deref(stored.NextFireAt), deref(got.NextFireAt))
	}
}

// TestAConditionWatchNeedsAProbeAndNoSchedule: a condition watch without a probe
// has nothing to observe, and one with a schedule has two triggers. Both are
// refused when the operator types them.
func TestAConditionWatchNeedsAProbeAndNoSchedule(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()

	noProbe := watchReq("no-probe")
	noProbe.Trigger, noProbe.Schedule = TriggerCondition, ""
	if _, err := CreateWatch(ctx, db, clock, noProbe); err == nil || !strings.Contains(err.Error(), "needs a probe") {
		t.Fatalf("condition without a probe = %v, want a refusal naming the probe", err)
	}

	scheduled := watchReq("scheduled")
	scheduled.Trigger, scheduled.Schedule, scheduled.Probe = TriggerCondition, "5m", "test -f /tmp/x"
	if _, err := CreateWatch(ctx, db, clock, scheduled); err == nil || !strings.Contains(err.Error(), "no schedule") {
		t.Fatalf("condition with a schedule = %v, want a refusal naming the schedule", err)
	}

	ok := watchReq("ok")
	ok.Trigger, ok.Schedule, ok.Probe = TriggerCondition, "", "test -f /tmp/x"
	if _, err := CreateWatch(ctx, db, clock, ok); err != nil {
		t.Fatalf("a valid condition watch was refused: %v", err)
	}
}

// TestAWatchTemplateIsValidatedAtCreate: nothing is written for a template the
// board would have to guess about.
func TestAWatchTemplateIsValidatedAtCreate(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	cases := []struct {
		name string
		edit func(*CreateWatchRequest)
		want string
	}{
		{"no title", func(r *CreateWatchRequest) { r.Template.Title = "" }, "title and a brief"},
		{"no brief", func(r *CreateWatchRequest) { r.Template.Brief = "" }, "title and a brief"},
		{"unknown capability", func(r *CreateWatchRequest) { r.Template.Needs = []string{"telepathy"} }, "needs"},
		{"negative priority", func(r *CreateWatchRequest) { r.Template.Priority = -1 }, "priority"},
		{"bad schedule", func(r *CreateWatchRequest) { r.Schedule = "soonish" }, "not a duration"},
		{"unknown trigger", func(r *CreateWatchRequest) { r.Trigger = "hourly" }, "trigger"},
		{"no name", func(r *CreateWatchRequest) { r.Name = "  " }, "needs a name"},
		{"negative dedupe", func(r *CreateWatchRequest) { d := -1; r.DedupeWindow = &d }, "negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := watchReq("w-" + tc.name)
			tc.edit(&req)
			if _, err := CreateWatch(ctx, db, clock, req); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	var watches, events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM watches`).Scan(&watches); err != nil {
		t.Fatalf("count watches: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE entity='watch'`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if watches != 0 || events != 0 {
		t.Fatalf("watches=%d events=%d after eight refusals, want nothing written", watches, events)
	}
}

// TestDuplicateWatchNamesAreRefused: names are how the operator refers to a watch,
// so a collision is a conflict (409 at the route), not a second row.
func TestDuplicateWatchNamesAreRefused(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	if _, err := CreateWatch(ctx, db, clock, watchReq("disk")); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if !errors.Is(err, ErrWatchExists) {
		t.Fatalf("err = %v, want ErrWatchExists", err)
	}
}

// TestTheWatchListIsOldestFirstAndEmptyIsNotNull: the list is [] before anything
// exists, and creation order after.
func TestTheWatchListIsOldestFirstAndEmptyIsNotNull(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()

	list, err := ListWatches(ctx, db)
	if err != nil {
		t.Fatalf("ListWatches: %v", err)
	}
	if list == nil || len(list) != 0 {
		t.Fatalf("empty list = %v, want a non-nil empty slice", list)
	}
	encoded, err := json.Marshal(list)
	if err != nil || string(encoded) != "[]" {
		t.Fatalf("empty list encodes as %s (err %v), want []", encoded, err)
	}

	first, err := CreateWatch(ctx, db, clock, watchReq("first"))
	if err != nil {
		t.Fatalf("first watch: %v", err)
	}
	clock.Advance(2 * time.Second)
	second, err := CreateWatch(ctx, db, clock, watchReq("second"))
	if err != nil {
		t.Fatalf("second watch: %v", err)
	}
	list, err = ListWatches(ctx, db)
	if err != nil {
		t.Fatalf("ListWatches: %v", err)
	}
	if len(list) != 2 || list[0].ID != first.ID || list[1].ID != second.ID {
		t.Fatalf("order = %v, want %s then %s", ids(list), first.ID, second.ID)
	}
}

func ids(list []Watch) []string {
	out := make([]string, 0, len(list))
	for _, w := range list {
		out = append(out, w.ID)
	}
	return out
}

// TestDisablingAWatchStopsItWithoutDeletingIt is D10: the row and its history stay.
func TestDisablingAWatchStopsItWithoutDeletingIt(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}

	off := false
	patched, err := PatchWatch(ctx, db, clock, w.ID, PatchWatchRequest{Enabled: &off})
	if err != nil {
		t.Fatalf("PatchWatch: %v", err)
	}
	if patched.Enabled {
		t.Fatal("the watch is still enabled")
	}
	list, err := ListWatches(ctx, db)
	if err != nil {
		t.Fatalf("ListWatches: %v", err)
	}
	if len(list) != 1 || list[0].ID != w.ID {
		t.Fatalf("list = %v, want the disabled watch still present", ids(list))
	}
}

// TestHoldingAWatchIsOperatorState: setting a hold, patching something else, and
// clearing the hold are three distinct operations, which is why the request's
// fields are pointers (D6).
func TestHoldingAWatchIsOperatorState(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}

	until := clock.Now() + 3600
	held, err := PatchWatch(ctx, db, clock, w.ID, PatchWatchRequest{CooldownUntil: &until})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if held.CooldownUntil == nil || *held.CooldownUntil != until {
		t.Fatalf("cooldown_until = %s, want %s", deref(held.CooldownUntil), stamp(until))
	}

	// A patch that names only enabled must leave the hold alone: that is the
	// difference between omitting a field and sending 0.
	off := false
	stillHeld, err := PatchWatch(ctx, db, clock, w.ID, PatchWatchRequest{Enabled: &off})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if stillHeld.CooldownUntil == nil || *stillHeld.CooldownUntil != until {
		t.Fatalf("cooldown_until = %s after an unrelated patch, want %s preserved", deref(stillHeld.CooldownUntil), stamp(until))
	}

	clear := int64(0)
	cleared, err := PatchWatch(ctx, db, clock, w.ID, PatchWatchRequest{CooldownUntil: &clear})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if cleared.CooldownUntil != nil {
		t.Fatalf("cooldown_until = %s after clearing, want nil", deref(cleared.CooldownUntil))
	}
}

// TestEnablingAStaleWatchStartsFromNow pins the decision the plan left open: a
// watch disabled across its own slots and then re-enabled starts a new schedule
// instead of paying the debt. D2's one-catch-up rule is about a manager that was
// down, not about an operator who said "not now".
func TestEnablingAStaleWatchStartsFromNow(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	on := true
	off := false
	if _, err := PatchWatch(ctx, db, clock, w.ID, PatchWatchRequest{Enabled: &off}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	clock.Advance(time.Hour) // three slots pass while it is disabled
	reEnabled, err := PatchWatch(ctx, db, clock, w.ID, PatchWatchRequest{Enabled: &on})
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if reEnabled.NextFireAt == nil || *reEnabled.NextFireAt != clock.Now()+300 {
		t.Fatalf("next_fire_at = %s after re-enabling, want %s (five minutes from now, not in the past)",
			deref(reEnabled.NextFireAt), stamp(clock.Now()+300))
	}
	if !reEnabled.Enabled {
		t.Fatal("the watch is not enabled after enabling it")
	}
}

// TestTheWatchRoutes walks the admin surface: create, list, read, the conflict on a
// duplicate name, the refusals a caller can cause, and the 401 a non-admin token
// gets. Watches decide what work appears, so the last one matters most.
func TestTheWatchRoutes(t *testing.T) {
	srv, _ := newServer(t)
	h := srv.Handler()

	if rec := doJSON(t, h, http.MethodGet, "/admin/watches", "not-the-admin", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-admin GET = %d, want 401", rec.Code)
	}

	rec := doJSON(t, h, http.MethodPost, "/admin/watches", testAdminToken, watchReq("disk"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var created Watch
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created watch: %v", err)
	}
	if created.ID == "" || created.NextFireAt == nil {
		t.Fatalf("created watch = %+v, want an id and a first fire", created)
	}

	rec = doJSON(t, h, http.MethodPost, "/admin/watches", testAdminToken, watchReq("disk"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate name = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, h, http.MethodPost, "/admin/watches", testAdminToken, map[string]string{"name": "x"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no trigger = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, h, http.MethodGet, "/admin/watches", testAdminToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", rec.Code)
	}
	var list []Watch
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %v, want the one watch", ids(list))
	}

	if rec := doJSON(t, h, http.MethodGet, "/admin/watches/"+created.ID, testAdminToken, nil); rec.Code != http.StatusOK {
		t.Fatalf("get = %d, want 200", rec.Code)
	}
	if rec := doJSON(t, h, http.MethodGet, "/admin/watches/nope", testAdminToken, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("get unknown = %d, want 404", rec.Code)
	}

	if rec := doJSON(t, h, http.MethodPatch, "/admin/watches/"+created.ID, testAdminToken, map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty patch = %d, want 400 (an empty body is not a no-op)", rec.Code)
	}
	if rec := doJSON(t, h, http.MethodPatch, "/admin/watches/nope", testAdminToken, map[string]any{"enabled": false}); rec.Code != http.StatusNotFound {
		t.Fatalf("patch unknown = %d, want 404", rec.Code)
	}
	rec = doJSON(t, h, http.MethodPatch, "/admin/watches/"+created.ID, testAdminToken, map[string]any{"enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// due advances the clock to the watch's next slot. Fire refuses an early call, so
// every legitimate fire in these tests has to arrive the way the scheduler does:
// with the clock at or past next_fire_at.
func due(t *testing.T, clock *FixedClock, w Watch) int64 {
	t.Helper()
	if w.NextFireAt == nil {
		t.Fatal("the watch has no next fire to advance to")
	}
	if d := *w.NextFireAt - clock.Now(); d > 0 {
		clock.Advance(time.Duration(d) * time.Second)
	}
	return *w.NextFireAt
}

// fireOne creates a watch and fires it once, returning the watch and the result.
func fireOne(t *testing.T, db *sql.DB, clock *FixedClock) (Watch, FireResult) {
	t.Helper()
	w, err := CreateWatch(context.Background(), db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	at := due(t, clock, w)
	res, err := Fire(context.Background(), db, clock, w.ID, at)
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	return w, res
}

// TestFiringEmitsAnItemWithTheTemplate: the item is the template, verbatim, and it
// belongs to the watch that emitted it. Everything downstream treats it like any
// other queued item (M4 D1).
func TestFiringEmitsAnItemWithTheTemplate(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	req := watchReq("disk")
	req.Template.Priority = 5
	req.Template.MaxAttempts = 2
	w, err := CreateWatch(context.Background(), db, clock, req)
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}

	at := due(t, clock, w)
	res, err := Fire(context.Background(), db, clock, w.ID, at)
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if !res.Fired || res.ItemID == "" {
		t.Fatalf("FireResult = %+v, want a fired item id", res)
	}

	var title, brief, needs, status, createdBy string
	var priority, maxAttempts int
	var claimedBy sql.NullString
	if err := db.QueryRow(
		`SELECT title,brief,needs,status,priority,max_attempts,created_by,claimed_by FROM work_items WHERE id=?`,
		res.ItemID).Scan(&title, &brief, &needs, &status, &priority, &maxAttempts, &createdBy, &claimedBy); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if title != req.Template.Title || brief != req.Template.Brief {
		t.Fatalf("item = %q / %q, want the template's title and brief", title, brief)
	}
	if needs != `["agentic"]` {
		t.Fatalf("needs = %s, want [\"agentic\"]", needs)
	}
	if status != "queued" || claimedBy.Valid {
		t.Fatalf("item status = %q claimed_by = %v, want a queued, unclaimed item", status, claimedBy)
	}
	if priority != 5 || maxAttempts != 2 {
		t.Fatalf("priority/max_attempts = %d/%d, want 5/2", priority, maxAttempts)
	}
	if createdBy != "watch:"+w.ID {
		t.Fatalf("created_by = %q, want watch:%s", createdBy, w.ID)
	}

	// The audit spine names the item and the trigger, not just "a watch fired".
	var payload string
	if err := db.QueryRow(
		`SELECT payload FROM events WHERE entity='watch' AND entity_id=? AND type='fired'`, w.ID).Scan(&payload); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if !strings.Contains(payload, res.ItemID) || !strings.Contains(payload, `"trigger":"interval"`) {
		t.Fatalf("fired payload = %s, want the item id and the trigger", payload)
	}
}

// TestFiringAdvancesTheScheduleAndStampsTheFire: one fire moves the watch to its
// next slot and records when it happened, so the next scan is about the next slot
// rather than the one just served.
func TestFiringAdvancesTheScheduleAndStampsTheFire(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	w, err := CreateWatch(context.Background(), db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	// The clock moves to the slot before the fire, so "advanced from now" and "left
	// at the observed value" are different numbers. With a frozen clock they are
	// equal and a fire that forgot to advance would pass.
	at := due(t, clock, w)
	res, err := Fire(context.Background(), db, clock, w.ID, at)
	if err != nil || !res.Fired {
		t.Fatalf("Fire = %+v, err %v; want a fire", res, err)
	}

	after, err := GetWatch(context.Background(), db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	if after.LastFiredAt == nil || *after.LastFiredAt != clock.Now() {
		t.Fatalf("last_fired_at = %s, want %s", deref(after.LastFiredAt), stamp(clock.Now()))
	}
	if after.NextFireAt == nil || *after.NextFireAt != clock.Now()+300 {
		t.Fatalf("next_fire_at = %s, want %s", deref(after.NextFireAt), stamp(clock.Now()+300))
	}
	var events int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE entity='watch' AND entity_id=? AND type='fired'`, w.ID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Fatalf("fired events = %d, want exactly 1", events)
	}
}

// TestAStaleScanCannotFireTwice is the guard: two schedulers (or one that scanned
// before another fired) both present the same next_fire_at, and only the first
// wins. The loser writes nothing — no item, no event, no advance.
func TestAStaleScanCannotFireTwice(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	w, err := CreateWatch(context.Background(), db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	observed := due(t, clock, w)

	first, err := Fire(context.Background(), db, clock, w.ID, observed)
	if err != nil || !first.Fired {
		t.Fatalf("first Fire = %+v, err %v; want a fire", first, err)
	}

	// Move the clock to the watch's NEW slot and hand Fire the token from the old
	// one. The watch is due, so only the token check can refuse this — which is
	// what the test is for.
	after, err := GetWatch(context.Background(), db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	clock.Advance(time.Duration(*after.NextFireAt-clock.Now()) * time.Second)

	second, err := Fire(context.Background(), db, clock, w.ID, observed)
	if err != nil {
		t.Fatalf("second Fire: %v", err)
	}
	if second.Fired {
		t.Fatalf("second Fire = %+v with a stale next_fire_at, want a skip", second)
	}
	if !strings.Contains(second.Skipped, "schedule moved") {
		t.Fatalf("second Fire skipped with %q, want it to name the moved schedule", second.Skipped)
	}

	var items, events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='fired'`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if items != 1 || events != 1 {
		t.Fatalf("items=%d fired events=%d after two evaluations of one slot, want 1 and 1", items, events)
	}
}

// TestADisabledWatchDoesNotFire: a scan that raced a disable must not emit. The
// disabled path is not an evaluation, so it also leaves the schedule alone — the
// re-enable is going to re-anchor it.
func TestADisabledWatchDoesNotFire(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	w, err := CreateWatch(context.Background(), db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	observed := due(t, clock, w)
	off := false
	if _, err := PatchWatch(context.Background(), db, clock, w.ID, PatchWatchRequest{Enabled: &off}); err != nil {
		t.Fatalf("PatchWatch: %v", err)
	}

	res, err := Fire(context.Background(), db, clock, w.ID, observed)
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if res.Fired || !strings.Contains(res.Skipped, "disabled") {
		t.Fatalf("Fire = %+v, want a disabled skip", res)
	}
	var items int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 0 {
		t.Fatalf("a disabled watch emitted %d items", items)
	}
}

// TestFiringDoesNotTouchAParkedItemOrItsGrant is M3's rule held across M4: the
// watch path creates items and does nothing else. A parked item keeps its request,
// its attempt and its lease-free state, and its run stays escalated.
func TestFiringDoesNotTouchAParkedItemOrItsGrant(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	itemID, runID, nodeID := parkOneRun(t, db, clock)
	if err := Escalate(context.Background(), db, clock, runID, nodeID, ApprovalRequest{
		Action: "delete /var/cache/app", Reason: "needs the disk",
	}, RunTokens{}); err != nil {
		t.Fatalf("Escalate: %v", err)
	}
	_ = itemID

	w, res := fireOne(t, db, clock)
	if !res.Fired {
		t.Fatalf("Fire = %+v, want a fire", res)
	}
	if w.ID == "" {
		t.Fatal("no watch")
	}
	var status, request string
	var attempt int
	if err := db.QueryRow(`SELECT status, COALESCE(approval_request,''), attempt FROM work_items WHERE id=?`, itemID).
		Scan(&status, &request, &attempt); err != nil {
		t.Fatalf("read parked item: %v", err)
	}
	if status != "needs_approval" || request == "" || attempt != 1 {
		t.Fatalf("parked item = %q, request %q, attempt %d; the watch path changed it", status, request, attempt)
	}
	var runStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE id=?`, runID).Scan(&runStatus); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if runStatus != "escalated" {
		t.Fatalf("run status = %q after a watch fired, want escalated", runStatus)
	}
}

// TestAFiredItemIsClaimable closes the loop M4 exists for: a watch emits work and
// the machinery that already exists picks it up. A watch whose items cannot be
// claimed is a spam generator (spec §18).
func TestAFiredItemIsClaimable(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")

	w, res := fireOne(t, db, clock)
	if !res.Fired {
		t.Fatalf("Fire = %+v, want a fire", res)
	}
	claimed, err := Claim(ctx, db, clock, node, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim = %d items, err %v; want the emitted item", len(claimed), err)
	}
	if claimed[0].Item.ID != res.ItemID {
		t.Fatalf("claimed %s, want the fired item %s", claimed[0].Item.ID, res.ItemID)
	}
	_ = w
}

// TestAFireThatIsNotDueIsRefused: Fire does not trust its scanner. A scheduler
// that called it for every watch — a bug, but a cheap one to guard against — must
// not be able to emit an item before the schedule says so.
func TestAFireThatIsNotDueIsRefused(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	res, err := Fire(ctx, db, clock, w.ID, *w.NextFireAt)
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if res.Fired {
		t.Fatalf("Fire = %+v for a watch due in five minutes, want a skip", res)
	}
	var items int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 0 {
		t.Fatalf("an early fire emitted %d items", items)
	}
}

// TestTwoFiresCannotBothEmitTheSameSlot is the guard's real test. The Go pre-check
// in Fire refuses a stale token on its own, so a single-threaded test cannot tell
// whether the SQL guard exists — and the guard is what protects two manager
// processes. Two connections on one file, both holding the same token, exactly one
// fire (the same shape as TestClaimIsExactlyOnceUnderConcurrency, and for the same
// reason: one pool would serialise the calls inside database/sql).
func TestTwoFiresCannotBothEmitTheSameSlot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	db, clock := OpenForTest(t, path)
	other, err := Open(path)
	if err != nil {
		t.Fatalf("open a second handle: %v", err)
	}
	t.Cleanup(func() { other.Close() })

	w, err := CreateWatch(context.Background(), db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	observed := due(t, clock, w)

	handles := []*sql.DB{db, other}
	results := make([]FireResult, len(handles))
	errs := make([]error, len(handles))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range handles {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = Fire(context.Background(), handles[i], clock, w.ID, observed)
		}(i)
	}
	close(start)
	wg.Wait()

	fired := 0
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("Fire(%d): %v", i, errs[i])
		}
		if res.Fired {
			fired++
		}
	}
	if fired != 1 {
		t.Fatalf("fired = %d of 2 concurrent fires of one slot (%+v), want exactly 1", fired, results)
	}

	var items, events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='fired'`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if items != 1 || events != 1 {
		t.Fatalf("items=%d fired events=%d after a concurrent race, want 1 and 1", items, events)
	}
}

// watchEventCount is how many events the watch itself has written. A skip must not
// change it: that is the property, and counting only "fired" would pass even if a
// skip wrote a "skipped" event per tick.
func watchEventCount(t *testing.T, db *sql.DB, watchID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE entity='watch' AND entity_id=?`, watchID).Scan(&n); err != nil {
		t.Fatalf("count watch events: %v", err)
	}
	return n
}

// hold sets a cooldown on a watch and returns nothing; it is fixture sugar for the
// skip tests.
func hold(t *testing.T, db *sql.DB, clock *FixedClock, id string, until int64) {
	t.Helper()
	if _, err := PatchWatch(context.Background(), db, clock, id, PatchWatchRequest{CooldownUntil: &until}); err != nil {
		t.Fatalf("hold: %v", err)
	}
}

// TestACooldownSkipsTheFireAndSaysSo: a held watch does not emit, says who is
// holding it and until when, and still advances its schedule so the scheduler is
// not re-asking every tick.
func TestACooldownSkipsTheFireAndSaysSo(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	until := clock.Now() + 3600
	hold(t, db, clock, w.ID, until)
	before := watchEventCount(t, db, w.ID)

	at := due(t, clock, w)
	res, err := Fire(ctx, db, clock, w.ID, at)
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if res.Fired {
		t.Fatalf("Fire = %+v for a held watch, want a skip", res)
	}
	if !strings.Contains(res.Skipped, "held by the operator") || !strings.Contains(res.Skipped, "2023-11-14T23:13:20Z") {
		t.Fatalf("skip reason = %q, want the hold and its expiry", res.Skipped)
	}

	after, err := GetWatch(ctx, db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	if after.NextFireAt == nil || *after.NextFireAt <= at {
		t.Fatalf("next_fire_at = %s after a skip, want it past the slot %s", deref(after.NextFireAt), stamp(at))
	}
	if after.LastSkipAt == nil || *after.LastSkipAt != clock.Now() {
		t.Fatalf("last_skip_at = %s, want %s", deref(after.LastSkipAt), stamp(clock.Now()))
	}
	if !strings.Contains(after.LastSkipReason, "held by the operator") {
		t.Fatalf("last_skip_reason = %q, want the reason stored on the row", after.LastSkipReason)
	}
	if after.LastFiredAt != nil {
		t.Fatalf("last_fired_at = %s after a skip, want nil", deref(after.LastFiredAt))
	}

	var items int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 0 {
		t.Fatalf("items = %d after a held slot, want nothing emitted", items)
	}
	if after := watchEventCount(t, db, w.ID); after != before {
		t.Fatalf("watch events went from %d to %d across a skip, want no event", before, after)
	}
}

// TestADedupeWindowSkipsAFireInsideTheWindow: the second emission inside the
// window waits, and the reason says which window and when it ends.
func TestADedupeWindowSkipsAFireInsideTheWindow(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	first, err := Fire(ctx, db, clock, w.ID, due(t, clock, w))
	if err != nil || !first.Fired {
		t.Fatalf("first Fire = %+v, err %v", first, err)
	}

	next, err := GetWatch(ctx, db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	at := due(t, clock, next) // five minutes later, inside the one-hour window
	res, err := Fire(ctx, db, clock, w.ID, at)
	if err != nil {
		t.Fatalf("second Fire: %v", err)
	}
	if res.Fired {
		t.Fatalf("second Fire = %+v inside the dedupe window, want a skip", res)
	}
	if !strings.Contains(res.Skipped, "dedupe window") {
		t.Fatalf("skip reason = %q, want the dedupe window", res.Skipped)
	}

	var items int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 1 {
		t.Fatalf("items = %d, want only the first fire's", items)
	}
}

// TestTheWindowDoesNotSkipAFireAfterIt fires exactly at the window's last second:
// the window is "at least this much time between emissions", so the boundary itself
// is allowed. `until >= now` would refuse it, which is why the test lands there.
func TestTheWindowDoesNotSkipAFireAfterIt(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	first, err := Fire(ctx, db, clock, w.ID, due(t, clock, w))
	if err != nil || !first.Fired {
		t.Fatalf("first Fire = %+v, err %v", first, err)
	}
	firedAt := clock.Now()

	// Jump to exactly last_fired + dedupe_window.
	clock.Advance(time.Duration(firedAt+3600-clock.Now()) * time.Second)
	current, err := GetWatch(ctx, db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	if clock.Now() != firedAt+3600 {
		t.Fatalf("the fixture is at %s, want the window boundary %s", stamp(clock.Now()), stamp(firedAt+3600))
	}
	at := due(t, clock, current)
	res, err := Fire(ctx, db, clock, w.ID, at)
	if err != nil {
		t.Fatalf("second Fire: %v", err)
	}
	if !res.Fired {
		t.Fatalf("second Fire = %+v at the window boundary, want a fire", res)
	}
}

// TestTheSkipReasonNamesTheFirstRefusal: held AND inside the window, the reason is
// the hold — the operator's decision outranks the system's floor, or they read
// about a window they did not set.
func TestTheSkipReasonNamesTheFirstRefusal(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	if first, err := Fire(ctx, db, clock, w.ID, due(t, clock, w)); err != nil || !first.Fired {
		t.Fatalf("first Fire = %+v, err %v", first, err)
	}
	hold(t, db, clock, w.ID, clock.Now()+7200)

	current, err := GetWatch(ctx, db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	res, err := Fire(ctx, db, clock, w.ID, due(t, clock, current))
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if !strings.Contains(res.Skipped, "held by the operator") {
		t.Fatalf("skip reason = %q, want the hold named first", res.Skipped)
	}
	if strings.Contains(res.Skipped, "dedupe") {
		t.Fatalf("skip reason = %q mentions the window as well; the first refusal has to win", res.Skipped)
	}
}

// TestASkipWritesNoEventPerTick: ten held slots are ten row updates, not ten
// entries in the audit spine. The board's answer to "why is it quiet" is the row.
func TestASkipWritesNoEventPerTick(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	hold(t, db, clock, w.ID, clock.Now()+86400)
	before := watchEventCount(t, db, w.ID)

	for i := 0; i < 10; i++ {
		current, err := GetWatch(ctx, db, w.ID)
		if err != nil {
			t.Fatalf("GetWatch: %v", err)
		}
		res, err := Fire(ctx, db, clock, w.ID, due(t, clock, current))
		if err != nil {
			t.Fatalf("Fire %d: %v", i, err)
		}
		if res.Fired {
			t.Fatalf("Fire %d = %+v for a held watch, want a skip", i, res)
		}
	}

	var items int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 0 {
		t.Fatalf("items = %d after ten held slots, want none", items)
	}
	if after := watchEventCount(t, db, w.ID); after != before {
		t.Fatalf("watch events went from %d to %d across ten skips, want none", before, after)
	}
	final, err := GetWatch(ctx, db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	if final.LastSkipAt == nil || !strings.Contains(final.LastSkipReason, "held") {
		t.Fatalf("last_skip = %s / %q, want the last skip recorded", deref(final.LastSkipAt), final.LastSkipReason)
	}
}

// conditionWatch builds a condition watch whose probe tests for a flag file, which
// the tests create and remove to move the condition between false and true.
func conditionWatch(t *testing.T, db *sql.DB, clock *FixedClock, flag string) Watch {
	t.Helper()
	req := watchReq("condition")
	req.Trigger, req.Schedule, req.Probe = TriggerCondition, "", "test -f "+flag
	// The dedupe floor is tested on its own (Task 5) and applies to every trigger by
	// D6's order — a probe that flaps at a threshold should not emit on every flip.
	// Zero here so the edge rule is what this test observes.
	noWindow := 0
	req.DedupeWindow = &noWindow
	w, err := CreateWatch(context.Background(), db, clock, req)
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	return w
}

// fireAgain re-reads the watch and fires it at its current slot. A condition watch's
// slot is "now" every time, so this is the next tick's evaluation.
func fireAgain(t *testing.T, db *sql.DB, clock *FixedClock, id string) FireResult {
	t.Helper()
	current, err := GetWatch(context.Background(), db, id)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	res, err := Fire(context.Background(), db, clock, id, due(t, clock, current))
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	return res
}

// TestAConditionFiresOnTheRisingEdgeOnly is D5: three checks of a true condition
// produce one item, and only a false→true transition produces the next.
func TestAConditionFiresOnTheRisingEdgeOnly(t *testing.T) {
	dir := t.TempDir()
	flag := filepath.Join(dir, "flag")
	db, clock := OpenForTest(t, filepath.Join(dir, "board.db"))
	w := conditionWatch(t, db, clock, flag)

	isFalse := fireAgain(t, db, clock, w.ID)
	if isFalse.Fired {
		t.Fatalf("first check = %+v with no flag, want a skip", isFalse)
	}
	if !strings.Contains(isFalse.Skipped, "condition is false") {
		t.Fatalf("first check skipped with %q, want the condition named", isFalse.Skipped)
	}
	afterFalse, err := GetWatch(context.Background(), db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	if afterFalse.LastResult == nil || *afterFalse.LastResult {
		t.Fatalf("last_result = %v after a false probe, want false", afterFalse.LastResult)
	}

	if err := os.WriteFile(flag, []byte("x"), 0o644); err != nil {
		t.Fatalf("write flag: %v", err)
	}
	rising := fireAgain(t, db, clock, w.ID)
	if !rising.Fired {
		t.Fatalf("rising edge = %+v, want a fire", rising)
	}

	for i := 0; i < 2; i++ {
		still := fireAgain(t, db, clock, w.ID)
		if still.Fired {
			t.Fatalf("check %d of a still-true condition fired again", i+1)
		}
		if !strings.Contains(still.Skipped, "already true") {
			t.Fatalf("still-true skip = %q, want the transition rule named", still.Skipped)
		}
	}

	if err := os.Remove(flag); err != nil {
		t.Fatalf("remove flag: %v", err)
	}
	falling := fireAgain(t, db, clock, w.ID)
	if falling.Fired {
		t.Fatal("a falling edge fired")
	}
	if err := os.WriteFile(flag, []byte("x"), 0o644); err != nil {
		t.Fatalf("write flag again: %v", err)
	}
	second := fireAgain(t, db, clock, w.ID)
	if !second.Fired {
		t.Fatalf("second rising edge = %+v, want a fire", second)
	}

	var items int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 2 {
		t.Fatalf("items = %d for two rising edges and four checks, want 2", items)
	}
}

// TestAConditionSurvivesARestart: the edge lives in the row, so a fresh manager that
// has never seen this watch still refuses to re-fire a condition that was already
// true. Simulated by writing last_result the way a previous process would have.
func TestAConditionSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	flag := filepath.Join(dir, "flag")
	if err := os.WriteFile(flag, []byte("x"), 0o644); err != nil {
		t.Fatalf("write flag: %v", err)
	}
	db, clock := OpenForTest(t, filepath.Join(dir, "board.db"))
	w := conditionWatch(t, db, clock, flag)
	if _, err := db.Exec(`UPDATE watches SET last_result=1 WHERE id=?`, w.ID); err != nil {
		t.Fatalf("seed last_result: %v", err)
	}

	res := fireAgain(t, db, clock, w.ID)
	if res.Fired {
		t.Fatalf("a condition already true in the row fired after a restart: %+v", res)
	}
	var items int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 0 {
		t.Fatalf("items = %d, want none", items)
	}
}

// TestAFailedProbeDoesNotFire: an interval watch's probe is a gate, and a failing
// gate says why — the exit code and the command's own words.
func TestAFailedProbeDoesNotFire(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	req := watchReq("disk")
	req.Probe = "echo boom >&2; exit 3"
	w, err := CreateWatch(context.Background(), db, clock, req)
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}

	res := fireAgain(t, db, clock, w.ID)
	if res.Fired {
		t.Fatalf("Fire = %+v with a failing probe, want a skip", res)
	}
	if !strings.Contains(res.Skipped, "exit 3") || !strings.Contains(res.Skipped, "boom") {
		t.Fatalf("skip = %q, want the exit code and the probe's output", res.Skipped)
	}
	after, err := GetWatch(context.Background(), db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	if after.LastResult == nil || *after.LastResult {
		t.Fatalf("last_result = %v after a failed probe, want false", after.LastResult)
	}
	var items int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 0 {
		t.Fatalf("items = %d, want none", items)
	}
}

// TestAProbeTimeoutIsNotAFalseObservation: a timeout is not an observation, so it
// must not move a condition's edge — otherwise a slow probe would look like the
// condition going false and then true again, firing a second item.
func TestAProbeTimeoutIsNotAFalseObservation(t *testing.T) {
	old := watchProbeTimeout
	watchProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { watchProbeTimeout = old })

	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	req := watchReq("slow")
	req.Trigger, req.Schedule, req.Probe = TriggerCondition, "", "sleep 2; true"
	w, err := CreateWatch(context.Background(), db, clock, req)
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	if _, err := db.Exec(`UPDATE watches SET last_result=1 WHERE id=?`, w.ID); err != nil {
		t.Fatalf("seed last_result: %v", err)
	}

	res := fireAgain(t, db, clock, w.ID)
	if res.Fired {
		t.Fatalf("Fire = %+v with a timing-out probe, want a skip", res)
	}
	if !strings.Contains(res.Skipped, "did not finish") {
		t.Fatalf("skip = %q, want the timeout named", res.Skipped)
	}
	after, err := GetWatch(context.Background(), db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	if after.LastResult == nil || !*after.LastResult {
		t.Fatalf("last_result = %v after a timeout, want the previous true observation untouched", after.LastResult)
	}
}

// TestProbeOutputIsTruncated: the tail is bounded, and the cut keeps it valid UTF-8
// because it ends up in JSON. The probe emits multi-byte runes so a byte cut would
// land mid-rune.
func TestProbeOutputIsTruncated(t *testing.T) {
	exit, tail, timedOut := runWatchProbe(context.Background(), "yes 'é' | head -c 9000", 5*time.Second)
	if timedOut {
		t.Fatal("the probe timed out")
	}
	if exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}
	if len(tail) > watchProbeTail {
		t.Fatalf("tail is %d bytes, want at most %d", len(tail), watchProbeTail)
	}
	if !utf8.ValidString(tail) {
		t.Fatal("the tail is not valid UTF-8; a byte-boundary cut split a rune")
	}
	if len(tail) < 1000 {
		t.Fatalf("tail is only %d bytes; the cap should keep a useful amount", len(tail))
	}
}

// TestTheSchedulerFiresEveryDueWatchInOnePass: one tick evaluates exactly the due
// set — the due watch fires, the held one is recorded as a skip, and the one whose
// slot is in the future is not touched at all.
func TestTheSchedulerFiresEveryDueWatchInOnePass(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()

	dueWatch, err := CreateWatch(ctx, db, clock, watchReq("due"))
	if err != nil {
		t.Fatalf("create due: %v", err)
	}
	heldWatch, err := CreateWatch(ctx, db, clock, watchReq("held"))
	if err != nil {
		t.Fatalf("create held: %v", err)
	}
	hold(t, db, clock, heldWatch.ID, clock.Now()+86400)

	clock.Advance(300 * time.Second) // the two above are due; the third is created after

	future, err := CreateWatch(ctx, db, clock, watchReq("future"))
	if err != nil {
		t.Fatalf("create future: %v", err)
	}

	pass, err := RunWatchesOnce(ctx, db, clock)
	if err != nil {
		t.Fatalf("RunWatchesOnce: %v", err)
	}
	if pass.Fired != 1 || pass.Skipped != 1 || pass.Failed != 0 {
		t.Fatalf("pass = %+v, want one fire and one skip", pass)
	}

	var items int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&items); err != nil {
		t.Fatalf("count items: %v", err)
	}
	if items != 1 {
		t.Fatalf("items = %d, want only the due watch's", items)
	}
	if current, err := GetWatch(ctx, db, future.ID); err != nil {
		t.Fatalf("GetWatch future: %v", err)
	} else if current.LastFiredAt != nil || current.LastSkipAt != nil {
		t.Fatalf("the future watch was evaluated: last_fired %v, last_skip %v", current.LastFiredAt, current.LastSkipAt)
	}
	if current, err := GetWatch(ctx, db, dueWatch.ID); err != nil {
		t.Fatalf("GetWatch due: %v", err)
	} else if current.LastFiredAt == nil {
		t.Fatal("the due watch did not record a fire")
	}
}

// TestOneBadWatchDoesNotStopThePass: a row the board cannot read is reported and
// counted, and the watch behind it still fires. The corrupt row is due first, so an
// implementation that returned at the first error would never reach the good one.
func TestOneBadWatchDoesNotStopThePass(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()

	// The corrupt watch is due now and sorts first (ORDER BY next_fire_at, id).
	now := clock.Now()
	if _, err := db.Exec(
		`INSERT INTO watches (id,name,trigger,schedule,probe,template,dedupe_window,enabled,next_fire_at,created_at)
		 VALUES ('a-corrupt','a-corrupt','interval','5m',NULL,'not json',3600,1,?,?)`,
		now, now); err != nil {
		t.Fatalf("insert corrupt watch: %v", err)
	}
	good, err := CreateWatch(ctx, db, clock, watchReq("good"))
	if err != nil {
		t.Fatalf("create good: %v", err)
	}
	clock.Advance(300 * time.Second)

	pass, err := RunWatchesOnce(ctx, db, clock)
	if err == nil {
		t.Fatal("the pass reported no error for a watch whose template cannot be read")
	}
	if !strings.Contains(err.Error(), "a-corrupt") {
		t.Fatalf("pass error = %q, want it to name the corrupt watch", err)
	}
	if pass.Failed != 1 || pass.Fired != 1 {
		t.Fatalf("pass = %+v, want one failure and one fire", pass)
	}
	if current, err := GetWatch(ctx, db, good.ID); err != nil {
		t.Fatalf("GetWatch: %v", err)
	} else if current.LastFiredAt == nil {
		t.Fatal("the good watch behind the corrupt row never fired")
	}
}

// TestAPausedBoardEmitsButDoesNotIssue is D9: the pause stops issuance, not
// observation. A watch that became due while the board is paused still emits — the
// event is history, and hiding it would make the pause rewrite what happened — and
// the item simply waits, because Claim refuses while paused.
func TestAPausedBoardEmitsButDoesNotIssue(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	node := SeedNodeForTest(t, db, clock, "a", "executor", "agentic")
	SeedEndpointForTest(t, db, clock, "local", "host", 0, "tools")

	if _, err := CreateWatch(ctx, db, clock, watchReq("due")); err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO config (key,value) VALUES ('board_paused','1')`); err != nil {
		t.Fatalf("pause the board: %v", err)
	}
	clock.Advance(300 * time.Second)

	pass, err := RunWatchesOnce(ctx, db, clock)
	if err != nil {
		t.Fatalf("RunWatchesOnce: %v", err)
	}
	if pass.Fired != 1 {
		t.Fatalf("pass = %+v, want the fire to happen while paused", pass)
	}
	var queued int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items WHERE status='queued'`).Scan(&queued); err != nil {
		t.Fatalf("count queued: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued = %d, want the emitted item waiting", queued)
	}
	claimed, err := Claim(ctx, db, clock, node, 1)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("Claim returned %d items on a paused board, want none", len(claimed))
	}
}

// TestTheSchedulerDoesNotTouchParkedItems: M3's rule survives a full pass. A parked
// item keeps its request, attempt and lease-free state while watches fire around it.
func TestTheSchedulerDoesNotTouchParkedItems(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()

	itemID, runID, nodeID := parkOneRun(t, db, clock)
	if err := Escalate(ctx, db, clock, runID, nodeID, ApprovalRequest{
		Action: "delete /var/cache/app", Reason: "needs the disk",
	}, RunTokens{}); err != nil {
		t.Fatalf("Escalate: %v", err)
	}
	if _, err := CreateWatch(ctx, db, clock, watchReq("due")); err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	clock.Advance(300 * time.Second)

	if pass, err := RunWatchesOnce(ctx, db, clock); err != nil || pass.Fired != 1 {
		t.Fatalf("pass = %+v, err %v; want one fire", pass, err)
	}
	var status, request string
	var attempt int
	if err := db.QueryRow(`SELECT status, COALESCE(approval_request,''), attempt FROM work_items WHERE id=?`, itemID).
		Scan(&status, &request, &attempt); err != nil {
		t.Fatalf("read parked item: %v", err)
	}
	if status != "needs_approval" || request == "" || attempt != 1 {
		t.Fatalf("parked item = %q, request %q, attempt %d; the scheduler changed it", status, request, attempt)
	}
}

// TestTheSchedulerLoopTicksAndStops: the ticker runs a pass without being asked and
// returns when its context is cancelled. The pass itself is tested above; this pins
// the loop's wiring, which is the part a test of RunWatchesOnce cannot see.
func TestTheSchedulerLoopTicksAndStops(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	if _, err := CreateWatch(ctx, db, clock, watchReq("due")); err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	clock.Advance(300 * time.Second)

	srv := NewServer(db, clock, nil, HashToken("admin"), HashToken("enroll"))
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.RunWatches(runCtx, 10*time.Millisecond) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM work_items`).Scan(&n); err != nil {
			t.Fatalf("count items: %v", err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the scheduler loop never fires the due watch")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunWatches did not return after its context was cancelled")
	}
}

// watchRowSnapshot renders every column of a watch as text, so a test can assert a
// read changed nothing without listing the columns a second time.
func watchRowSnapshot(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM watches WHERE id=?`, id)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("snapshot columns: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("snapshot: no watch %s", id)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatalf("snapshot scan: %v", err)
	}
	var b strings.Builder
	for i, c := range cols {
		fmt.Fprintf(&b, "%s=%v;", c, vals[i])
	}
	return b.String()
}

// TestAWatchStateNamesTheNextSlot: a healthy schedule watch says when it will look
// again, in the same format the skip reasons use.
func TestAWatchStateNamesTheNextSlot(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	w, err := CreateWatch(context.Background(), db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	state := WatchState(w, clock.Now())
	// It has not fired yet, so the sentence is about its first slot; what matters is
	// that the slot's own timestamp is there to compare with the schedule.
	if !strings.Contains(state, stamp(*w.NextFireAt)) || !strings.Contains(state, "slot") {
		t.Fatalf("state = %q, want the slot %s named", state, stamp(*w.NextFireAt))
	}
	if strings.Contains(state, "not running") {
		t.Fatalf("state = %q claims a stopped scheduler for a healthy watch", state)
	}
}

// TestAWatchStateNamesAnOverdueSchedule is the sentence that matters: a slot ten
// minutes in the past is not "waiting", it is a manager that is not running.
func TestAWatchStateNamesAnOverdueSchedule(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	// The row a stopped manager leaves behind: the slot came and went, nothing
	// advanced it.
	overdueAt := clock.Now() - 600
	if _, err := db.Exec(`UPDATE watches SET next_fire_at=? WHERE id=?`, overdueAt, w.ID); err != nil {
		t.Fatalf("make overdue: %v", err)
	}
	got, err := GetWatch(ctx, db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}

	state := WatchState(got, clock.Now())
	if !strings.Contains(state, "was due at "+stamp(overdueAt)) {
		t.Fatalf("state = %q, want the slot it missed (%s)", state, stamp(overdueAt))
	}
	if !strings.Contains(state, "scheduler is not running or is stuck") {
		t.Fatalf("state = %q, want the scheduler named as the cause", state)
	}
	if strings.Contains(state, "waiting") {
		t.Fatalf("state = %q calls an overdue watch waiting", state)
	}
}

// TestAWatchStateNamesAnOverdueSlotAsImminentInsideTheGrace: a slot a few seconds
// old is the next pass's business, not a stopped manager. Without the grace every
// watch would read as broken for fifteen seconds per slot.
func TestAWatchStateNamesAnOverdueSlotAsImminentInsideTheGrace(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	if _, err := db.Exec(`UPDATE watches SET next_fire_at=? WHERE id=?`, clock.Now()-5, w.ID); err != nil {
		t.Fatalf("make due: %v", err)
	}
	got, err := GetWatch(ctx, db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	state := WatchState(got, clock.Now())
	if !strings.Contains(state, "the scheduler's next pass will take it") {
		t.Fatalf("state = %q, want the next pass named", state)
	}
	if strings.Contains(state, "not running") {
		t.Fatalf("state = %q calls a five-second-old slot a stopped scheduler", state)
	}
}

// TestAWatchStateNamesTheHoldBeforeTheSchedule: held and overdue is still "held".
// The operator's decision is the answer; the missed slot is a consequence of it.
func TestAWatchStateNamesTheHoldBeforeTheSchedule(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	until := clock.Now() + 7200
	hold(t, db, clock, w.ID, until)
	if _, err := db.Exec(`UPDATE watches SET next_fire_at=? WHERE id=?`, clock.Now()-600, w.ID); err != nil {
		t.Fatalf("make overdue: %v", err)
	}
	got, err := GetWatch(ctx, db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	state := WatchState(got, clock.Now())
	if !strings.Contains(state, "held by the operator until "+stamp(until)) {
		t.Fatalf("state = %q, want the hold (%s)", state, stamp(until))
	}
	if strings.Contains(state, "scheduler is not running") {
		t.Fatalf("state = %q diagnoses a stopped scheduler when the operator held the watch", state)
	}
}

// TestAWatchStateSaysDisabledFirst: a disabled watch's schedule is frozen, so naming
// a hold or a slot would describe something nobody is consulting.
func TestAWatchStateSaysDisabledFirst(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	off := false
	if _, err := PatchWatch(ctx, db, clock, w.ID, PatchWatchRequest{Enabled: &off}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	hold(t, db, clock, w.ID, clock.Now()+7200)
	got, err := GetWatch(ctx, db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	state := WatchState(got, clock.Now())
	if !strings.Contains(state, "disabled") || strings.Contains(state, "held") {
		t.Fatalf("state = %q, want disabled named and the hold left out", state)
	}
}

// TestAConditionStateNamesTheObservation: a condition watch's next_fire_at is always
// "now", so the overdue rule must not apply — a healthy one would read as a stopped
// scheduler. The observation, and how long ago it was taken, is the answer.
func TestAConditionStateNamesTheObservation(t *testing.T) {
	dir := t.TempDir()
	db, clock := OpenForTest(t, filepath.Join(dir, "board.db"))
	ctx := context.Background()
	w := conditionWatch(t, db, clock, filepath.Join(dir, "flag"))

	// Never checked: no observation, no claim about the scheduler.
	if state := WatchState(w, clock.Now()); !strings.Contains(state, "has not been checked yet") {
		t.Fatalf("state = %q for an unchecked condition, want that said", state)
	}

	// A false observation, taken just now.
	res := fireAgain(t, db, clock, w.ID)
	if res.Fired {
		t.Fatalf("an unset flag fired: %+v", res)
	}
	current, err := GetWatch(ctx, db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	state := WatchState(current, clock.Now())
	if !strings.Contains(state, "the condition is false") {
		t.Fatalf("state = %q, want the observation", state)
	}
	if strings.Contains(state, "not running") {
		t.Fatalf("state = %q calls a just-checked condition a stopped scheduler", state)
	}

	// The same row, ten minutes later: nothing looked at it.
	stale := WatchState(current, clock.Now()+600)
	if !strings.Contains(stale, "scheduler is not running") {
		t.Fatalf("state = %q for a condition unchecked for ten minutes, want the scheduler named", stale)
	}
}

// TestReadingAWatchWritesNothing is the /why rule again: an explanation must not
// change the thing it explains. Both forms are read — the row and its sentence — and
// the row, the event count and the second response are compared before and after.
func TestReadingAWatchWritesNothing(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	srv := NewServer(db, clock, nil, HashToken(testAdminToken), HashToken(testEnrollToken))
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO config (key,value) VALUES ('board_paused','1')`); err != nil {
		t.Fatalf("pause: %v", err)
	}

	before := watchRowSnapshot(t, db, w.ID)
	var eventsBefore int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&eventsBefore); err != nil {
		t.Fatalf("count events: %v", err)
	}

	rec := do(t, srv.Handler(), "GET", "/admin/watches/"+w.ID, testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body WatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.State == "" {
		t.Fatal("the response carries no state sentence")
	}

	again := do(t, srv.Handler(), "GET", "/admin/watches/"+w.ID, testAdminToken)
	if again.Body.String() != rec.Body.String() {
		t.Fatalf("two reads differ:\n%s\n%s", rec.Body.String(), again.Body.String())
	}
	if after := watchRowSnapshot(t, db, w.ID); after != before {
		t.Fatalf("the row changed across a read:\nbefore %s\nafter  %s", before, after)
	}
	var eventsAfter int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&eventsAfter); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventsAfter != eventsBefore {
		t.Fatalf("events went from %d to %d across a read", eventsBefore, eventsAfter)
	}
}

// TestTheListCarriesTheState: a list an operator scans has to be readable without a
// request per row.
func TestTheListCarriesTheState(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	srv := NewServer(db, clock, nil, HashToken(testAdminToken), HashToken(testEnrollToken))
	if _, err := CreateWatch(ctx, db, clock, watchReq("a")); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := CreateWatch(ctx, db, clock, watchReq("b")); err != nil {
		t.Fatalf("create b: %v", err)
	}
	rec := do(t, srv.Handler(), "GET", "/admin/watches", testAdminToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var views []WatchView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("got %d watches, want 2", len(views))
	}
	for _, v := range views {
		if !strings.Contains(v.State, stamp(*v.NextFireAt)) {
			t.Fatalf("watch %s state = %q, want its slot %s", v.Name, v.State, stamp(*v.NextFireAt))
		}
	}
}

// TestAWatchStateCarriesOnlyACurrentSkip: a skip older than the last fire describes
// a slot that has since been overtaken. Printing it would tell an operator about a
// refusal that no longer applies.
func TestAWatchStateCarriesOnlyACurrentSkip(t *testing.T) {
	db, clock := OpenForTest(t, filepath.Join(t.TempDir(), "board.db"))
	ctx := context.Background()
	w, err := CreateWatch(ctx, db, clock, watchReq("disk"))
	if err != nil {
		t.Fatalf("CreateWatch: %v", err)
	}

	if first := fireAgain(t, db, clock, w.ID); !first.Fired {
		t.Fatalf("first fire = %+v", first)
	}
	fired, err := GetWatch(ctx, db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	if state := WatchState(fired, clock.Now()); strings.Contains(state, "last skipped") {
		t.Fatalf("state = %q narrates a skip that never happened", state)
	}

	// Hold it, then let a slot pass: that is a skip, and the newest event of the two.
	hold(t, db, clock, w.ID, clock.Now()+7200)
	clock.Advance(300 * time.Second)
	if res := fireAgain(t, db, clock, w.ID); res.Fired {
		t.Fatalf("a held slot fired: %+v", res)
	}
	// Released without moving the clock: the hold no longer explains the quiet, so the
	// sentence falls through to the schedule — and the skip it narrates is the current
	// one, newer than the last fire.
	if _, err := PatchWatch(ctx, db, clock, w.ID, PatchWatchRequest{CooldownUntil: ptrInt64(0)}); err != nil {
		t.Fatalf("release: %v", err)
	}
	released, err := GetWatch(ctx, db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	if state := WatchState(released, clock.Now()); !strings.Contains(state, "last skipped at "+stamp(*released.LastSkipAt)) {
		t.Fatalf("state = %q, want the current skip narrated", state)
	}

	// Fire again, past the dedupe window. The skip is now older than the fire and must
	// stop being narrated.
	clock.Advance(time.Duration(*released.LastFiredAt+3600-clock.Now()+60) * time.Second)
	if res := fireAgain(t, db, clock, w.ID); !res.Fired {
		t.Fatalf("the fire after the hold = %+v", res)
	}
	final, err := GetWatch(ctx, db, w.ID)
	if err != nil {
		t.Fatalf("GetWatch: %v", err)
	}
	if state := WatchState(final, clock.Now()); strings.Contains(state, "last skipped") {
		t.Fatalf("state = %q still narrates an overtaken skip", state)
	}
}

// ptrInt64 is fixture sugar for the PatchWatchRequest's pointer fields.
func ptrInt64(v int64) *int64 { return &v }
