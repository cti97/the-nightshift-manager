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
