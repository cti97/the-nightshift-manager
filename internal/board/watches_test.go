// internal/board/watches_test.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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
