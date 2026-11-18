package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func newTestHermesObserver(t *testing.T) *hermesObserver {
	t.Helper()
	dir := t.TempDir()
	pending := filepath.Join(dir, "pending")
	if err := os.Mkdir(pending, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pending, 0o750|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	o, err := newHermesObserver(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.close)
	return o
}

func TestHermesObserverOptInRequiresProvisionedDedicatedDirectory(t *testing.T) {
	t.Setenv(hermesObserverEnableEnv, "true")
	t.Setenv(hermesObserverDirEnv, "")
	if got := newHermesObserverFromEnv(); got != nil {
		got.close()
		t.Fatal("observer must stay disabled unless the environment is exactly 1")
	}

	t.Setenv(hermesObserverEnableEnv, "1")
	if got := newHermesObserverFromEnv(); got != nil {
		got.close()
		t.Fatal("observer must require an explicitly provisioned directory")
	}

	t.Setenv(hermesObserverDirEnv, "relative-observer-dir")
	if got := newHermesObserverFromEnv(); got != nil {
		got.close()
		t.Fatal("observer must reject a relative directory")
	}

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "pending"), 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "pending"), 0o770|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	t.Setenv(hermesObserverDirEnv, dir)
	if got := newHermesObserverFromEnv(); got != nil {
		got.close()
		t.Fatal("observer must reject a sender-writable pending directory")
	}
	if err := os.Chmod(filepath.Join(dir, "pending"), 0o750|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	got := newHermesObserverFromEnv()
	if got == nil {
		t.Fatal("observer must enable with an explicit pre-provisioned directory")
	}
	got.close()
	// Startup/configuration must not create an outbox tree itself.
	entries, err := os.ReadDir(filepath.Join(dir, "pending"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("observer setup wrote receipt paths: entries=%v err=%v", entries, err)
	}
}

func TestHermesObserverActivityReceiptIsMetadataOnlyAndReceiverDeduplicable(t *testing.T) {
	o := newTestHermesObserver(t)
	now := time.Date(2026, 11, 17, 11, 22, 33, 444000000, time.UTC)
	o.now = func() time.Time { return now }

	const summary = "https://alice:super-secret@example.test/private?token=should-not-leak"
	const detail = "Authorization: Bearer top-secret\n/home/operator/private/source.rs"
	const metadata = `{"request":"Cookie: session=do-not-copy","host":"target.example.test"}`
	nodeID := int64(88)
	o.emitActivity(&Task{ID: "42", ExpID: 7}, db.Activity{
		ID: 99, NodeID: &nodeID, Worker: "worker-1", Kind: "tool_use", Tool: "curl <secret>", ToolUseID: "call-1",
		IsError: true, Summary: summary, Detail: detail, Metadata: json.RawMessage(metadata), CreatedAt: now,
	})

	receipt, path := readOnlyHermesReceipt(t, o.pendingPath)
	if receipt.Version != 1 || receipt.Kind != "activity.persisted" || receipt.CreatedAt != now.Format(time.RFC3339Nano) {
		t.Fatalf("unexpected receipt header: %+v", receipt)
	}
	if !strings.HasPrefix(filepath.Base(path), "event-") || !strings.HasSuffix(path, ".json") || filepath.Base(path) != receipt.ID+".json" {
		t.Fatalf("receipt filename must match opaque event id, got %q / %q", filepath.Base(path), receipt.ID)
	}
	for _, want := range []string{
		"timestamp=" + now.Format(time.RFC3339Nano),
		"event_id=" + receipt.ID,
		"event=activity.persisted",
		"task_id=42", "exploration_id=7", "activity_id=99", "node_id=88",
		"kind=tool_use", "worker_role=worker", "tool_present=true", "is_error=true",
	} {
		if !strings.Contains(receipt.Message, want) {
			t.Fatalf("receipt message missing %q: %s", want, receipt.Message)
		}
	}
	for _, forbidden := range []string{
		summary, detail, metadata, "Authorization", "super-secret", "top-secret", "/home/operator", "session=do-not-copy",
		"sha256", "_bytes=", "summary_", "detail_", "metadata_",
	} {
		if strings.Contains(receipt.Message, forbidden) {
			t.Fatalf("receipt leaked content-derived value %q: %s", forbidden, receipt.Message)
		}
	}

	assertMode(t, o.pendingPath, 0o750)
	assertMode(t, path, 0o640)
	if tmp, err := filepath.Glob(filepath.Join(o.pendingPath, ".event-*.tmp")); err != nil || len(tmp) != 0 {
		t.Fatalf("atomic publish left temp files %v (%v)", tmp, err)
	}
}

func TestHermesObserverTaskCreatedAndBoundedPendingEvictsOldest(t *testing.T) {
	o := newTestHermesObserver(t)
	o.maxPending = 2
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	o.now = func() time.Time { return now }

	o.emitTaskCreated(&Task{ID: "17", ExpID: 31, Description: "private description", Goal: "private goal"})
	// Duplicate lifecycle notifications in the same process are ignored.
	o.emitTaskCreated(&Task{ID: "17", ExpID: 31, Description: "different", Goal: "different"})
	// The third event makes the bounded producer queue evict its oldest receipt;
	// the feed stays live even if the sender has stopped.
	o.emitActivity(&Task{ID: "17", ExpID: 31}, db.Activity{ID: 1, Kind: "result", Summary: "private", CreatedAt: now.Add(time.Nanosecond)})
	o.emitActivity(&Task{ID: "17", ExpID: 31}, db.Activity{ID: 2, Kind: "result", Summary: "must not leak", CreatedAt: now.Add(2 * time.Nanosecond)})

	// The observer publishes asynchronously. A count of two alone is a valid
	// transient state after the first activity is written but before the second
	// activity performs the eviction. Wait for the promised retained receipts,
	// not merely the intermediate bounded count.
	entries := waitForHermesActivityReceiptIDs(t, o.pendingPath, "1", "2")
	if len(entries) != 2 {
		t.Fatalf("outbox must be bounded at two pending receipts, got %d", len(entries))
	}
	activityIDs := map[string]bool{}
	for _, entry := range entries {
		blob, err := os.ReadFile(filepath.Join(o.pendingPath, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var r hermesObserverReceipt
		if err := json.Unmarshal(blob, &r); err != nil {
			t.Fatal(err)
		}
		if r.Kind != "activity.persisted" {
			t.Fatalf("oldest task-created receipt was not evicted: %+v", r)
		}
		if strings.Contains(r.Message, "private") || strings.Contains(r.Message, "must not leak") {
			t.Fatalf("bounded receipt leaked raw content: %+v", r)
		}
		for _, id := range []string{"1", "2"} {
			if strings.Contains(r.Message, "activity_id="+id) {
				activityIDs[id] = true
			}
		}
	}
	if !activityIDs["1"] || !activityIDs["2"] {
		t.Fatalf("retained activity IDs=%v, want 1 and 2", activityIDs)
	}
}

func TestHermesObserverRequiresPersistedActivityID(t *testing.T) {
	o := newTestHermesObserver(t)
	o.emitActivity(&Task{ID: "7", ExpID: 8}, db.Activity{Kind: "text", Summary: "must not appear"})
	entries, err := os.ReadDir(o.pendingPath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("activity without persisted id must not create a receipt: entries=%v err=%v", entries, err)
	}
}

func TestHermesObserverQueueDropsWithoutBlockingAndDoesNotPremarkLifecycle(t *testing.T) {
	o := newTestHermesObserver(t)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	writerStarted := false
	o.writeReceipt = func(hermesObserverEvent) bool {
		if !writerStarted {
			writerStarted = true
			close(started)
		}
		<-release
		return true
	}

	// The first event is consumed by the sole writer, which then waits. Fill its
	// bounded queue behind it; no ARTEX call should touch disk or wait for that
	// stalled writer.
	o.emitActivity(&Task{ID: "1", ExpID: 2}, db.Activity{ID: 1, Kind: "text"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("observer writer did not start")
	}
	start := time.Now()
	for i := 0; i < hermesObserverQueueCapacity+8; i++ {
		o.emitActivity(&Task{ID: "1", ExpID: 2}, db.Activity{ID: int64(i + 2), Kind: "text"})
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("bounded observer enqueue blocked for %s behind its writer", elapsed)
	}
	if o.dropped.Load() == 0 {
		t.Fatal("queue saturation did not drop metadata")
	}

	// A task-created notification dropped while the queue is full must not mark
	// the task as sent: a later lifecycle call can enqueue it after recovery.
	o.emitTaskCreated(&Task{ID: "911", ExpID: 12})
	o.createdMu.Lock()
	_, marked := o.createdTasks["911"]
	o.createdMu.Unlock()
	if marked {
		t.Fatal("full observer queue permanently suppressed task-created event")
	}
}

func TestHermesObserverDropsNonNumericTaskIDsWithoutDigest(t *testing.T) {
	for _, raw := range []string{"", " with space", "leaked_secret", "42/secret", "-1", strings.Repeat("9", 64)} {
		if got := hermesTaskID(raw); got != "-" {
			t.Fatalf("hermesTaskID(%q)=%q, want -", raw, got)
		}
	}
	if got := hermesTaskID("00042"); got != "42" {
		t.Fatalf("numeric task id=%q, want 42", got)
	}
	if got := hermesNonnegativeID(-1); got != "0" {
		t.Fatalf("negative database id=%q, want 0", got)
	}
	if got := hermesActivityKind("private-target.example"); got != "-" {
		t.Fatalf("unsafe activity kind=%q, want -", got)
	}
	if got := hermesWorkerRole("private-agent-name"); got != "-" {
		t.Fatalf("custom worker role=%q, want -", got)
	}
	if got := hermesWorkerRole("worker-private-target"); got != "worker" {
		t.Fatalf("worker role class=%q, want worker", got)
	}
	message := hermesTaskCreatedMessage("event-01763400000000000000-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", &Task{ID: "leaked_secret", ExpID: 7}, time.Now())
	if strings.Contains(message, "leaked_secret") || !strings.Contains(message, "task_id=-") {
		t.Fatalf("unsafe task id escaped into receipt message: %s", message)
	}
}

func readOnlyHermesReceipt(t *testing.T, dir string) (hermesObserverReceipt, string) {
	t.Helper()
	entries := waitForHermesReceiptCount(t, dir, 1)
	path := filepath.Join(dir, entries[0].Name())
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt hermesObserverReceipt
	if err := json.Unmarshal(blob, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt, path
}

func waitForHermesReceiptCount(t *testing.T, dir string, want int) []os.DirEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		receipts := make([]os.DirEntry, 0, len(entries))
		for _, entry := range entries {
			name := entry.Name()
			// writeEvent publishes through a dot-prefixed temporary name. A
			// directory snapshot can observe that valid intermediate state, but
			// the host sender and this receipt-level test must see only the
			// final event-*.json contract.
			if !strings.HasPrefix(name, "event-") || !strings.HasSuffix(name, ".json") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().IsRegular() {
				receipts = append(receipts, entry)
			}
		}
		if len(receipts) == want {
			return receipts
		}
		if time.Now().After(deadline) {
			t.Fatalf("want exactly %d published receipt(s), got %d (directory entries=%d)", want, len(receipts), len(entries))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForHermesActivityReceiptIDs waits for the final bounded-outbox contract
// rather than a count that can be true while the single observer writer is
// still processing a later queued event. It is test-only; production eviction
// remains asynchronous and intentionally lossy.
func waitForHermesActivityReceiptIDs(t *testing.T, dir string, wantIDs ...string) []os.DirEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		receipts := make([]os.DirEntry, 0, len(entries))
		seen := make(map[string]bool, len(wantIDs))
		allActivity := true
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, "event-") || !strings.HasSuffix(name, ".json") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() {
				continue
			}
			blob, err := os.ReadFile(filepath.Join(dir, name))
			if os.IsNotExist(err) {
				allActivity = false
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			var receipt hermesObserverReceipt
			if err := json.Unmarshal(blob, &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.Kind != "activity.persisted" {
				allActivity = false
				continue
			}
			for _, id := range wantIDs {
				if strings.Contains(receipt.Message, "activity_id="+id) {
					seen[id] = true
				}
			}
			receipts = append(receipts, entry)
		}
		if allActivity && len(receipts) == len(wantIDs) && len(seen) == len(wantIDs) {
			return receipts
		}
		if time.Now().After(deadline) {
			t.Fatalf("want final activity receipts %v, got %d receipt(s) with ids=%v (directory entries=%d)", wantIDs, len(receipts), seen, len(entries))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode=%#o, want %#o", path, got, want)
	}
}

func TestHermesObserverEventIDUsesCanonicalProducerFormat(t *testing.T) {
	o := newTestHermesObserver(t)
	o.random = strings.NewReader(strings.Repeat("x", 16))
	at := time.Date(2026, 11, 17, 13, 0, 0, 0, time.UTC)
	id, ok := o.nextID(at)
	if !ok {
		t.Fatal("nextID failed")
	}
	wantPrefix := fmt.Sprintf("event-%020d-", at.UTC().UnixNano())
	want := wantPrefix + strings.Repeat("78", 16)
	if id != want {
		t.Fatalf("id=%q, want %q", id, want)
	}
}
