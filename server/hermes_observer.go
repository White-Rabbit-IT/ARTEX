ýë\þšè~)^wø¦r‰ßŠÊV§vÈeŠw¹þë+þÈZ­ïâ™Ê'~(?Šx­‰©bÍêÍ¢Ëœ…ø¥zŠÝŠ·œ¶Šòpackage server

// The Hermes observer is deliberately an outbox, not a Hermes client. ARTEX
// writes a small, metadata-only receipt locally; a host-side operator process
// is the only component allowed to decide whether and how to send it to Hermes.
// That separation keeps OAuth/session material, network access, and private
// research content out of the ARTEX container.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/db"
)

const (
	hermesObserverEnableEnv  = "ARTEX_HERMES_OBSERVER_ENABLE"
	hermesObserverDirEnv     = "ARTEX_HERMES_OBSERVER_DIR"
	hermesObserverMaxPending = 1024
	// A full observer queue intentionally drops newest status metadata rather
	// than making a persisted activity, planner, or worker wait for filesystem
	// I/O. The database/local evidence system remains authoritative.
	hermesObserverQueueCapacity = 256
	// A stuck filesystem must not hold ARTEX shutdown indefinitely. Normal
	// shutdown joins the single writer; after this short bound it is left to
	// finish and close its Root asynchronously, which is safe because Root is
	// never closed while its writer can still use it.
	hermesObserverShutdownWait = 100 * time.Millisecond
)

// hermesObserverReceipt is the narrow hand-off contract with the host-side
// sender. The sender only reads a pending receipt. It records successful
// delivery in its own user-owned state directory instead of moving or changing
// ARTEX files.
//
// Message intentionally contains only a compact, sanitized metadata line. In
// particular it never contains activity Summary, Detail, Metadata, their
// digests or sizes, a target, task description/goal, command output, paths,
// headers, prompts, or secrets.
type hermesObserverReceipt struct {
	Version   int    `json:"version"`
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Kind      string `json:"kind"`
	Message   string `json:"message"`
}

// hermesObserverEvent is the in-memory form of the narrow receipt contract.
// It contains only already-sanitized primitive metadata. In particular it does
// not retain a Task, Activity, summary, tool text, or any other content that
// could be serialized later by the asynchronous writer.
type hermesObserverEvent struct {
	kind          string
	at            time.Time
	taskID        string
	explorationID string
	activityID    string
	nodeID        string
	activityKind  string
	workerRole    string
	toolPresent   bool
	isError       bool
}

// hermesObserver owns a bounded in-memory queue and a bounded on-disk outbox.
// One writer goroutine is the only code that performs filesystem I/O. The
// planner/worker path only tries a non-blocking queue send, so observer trouble
// cannot delay or change ARTEX task execution. The outbox uses a pre-provisioned
// pending directory opened as an os.Root. ARTEX never chowns, changes the mode
// of, or creates observer directories: deployment provisions a producer-owned,
// sender-readable directory outside the general ARTEX data tree.
type hermesObserver struct {
	pending     *os.Root
	pendingPath string // diagnostic/test-only; all I/O uses pending
	now         func() time.Time
	random      io.Reader
	maxPending  int

	queue chan hermesObserverEvent
	stop  chan struct{}
	done  chan struct{}

	closeOnce        sync.Once
	closePendingOnce sync.Once
	createdMu        sync.Mutex
	createdTasks     map[string]struct{} // task id -> struct{}, avoids duplicate lifecycle receipts in one process
	dropped          atomic.Uint64

	// writeReceipt is normally writeEvent. Tests can substitute a bounded
	// deterministic sink to prove a stuck writer does not block task emission.
	// It is initialized before run starts and only changed before a test emits.
	writeReceipt func(hermesObserverEvent) bool
}

func newHermesObserverFromEnv() *hermesObserver {
	// Deliberately exact: this facility is off unless the operator explicitly
	// chose ARTEX_HERMES_OBSERVER_ENABLE=1.
	if os.Getenv(hermesObserverEnableEnv) != "1" {
		return nil
	}
	dir := strings.TrimSpace(os.Getenv(hermesObserverDirEnv))
	if dir == "" {
		log.Printf("[hermes-observer] disabled: %s=1 requires an explicitly provisioned %s", hermesObserverEnableEnv, hermesObserverDirEnv)
		return nil
	}
	o, err := newHermesObserver(dir)
	if err != nil {
		log.Printf("[hermes-observer] disabled: invalid dedicated observer directory: %v", err)
		return nil
	}
	return o
}

// newHermesObserver opens an existing deployment-owned directory. The caller
// must pre-provision <dir>/pending with a non-group/non-world-writable mode.
// Using a Root anchored at that directory ensures receipt operations cannot
// follow an attacker-supplied path outside it, even if a parent path changes
// after startup. No ownership or mode changes occur here.
func newHermesObserver(dir string) (*hermesObserver, error) {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if !filepath.IsAbs(dir) || dir == "/" {
		return nil, fmt.Errorf("observer directory must be an absolute non-root path")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open observer directory: %w", err)
	}
	closeRoot := true
	defer func() {
		if closeRoot {
			_ = root.Close()
		}
	}()

	rootInfo, err := root.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("stat observer directory: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("observer directory must be a non-group/non-world-writable directory")
	}
	info, err := root.Lstat("pending")
	if err != nil {
		return nil, fmt.Errorf("observer pending directory is not provisioned: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("observer pending path must be a real directory")
	}
	// The Hermes sender has read-and-search access through a dedicated group,
	// but it must not be able to replace directory entries that ARTEX writes.
	if info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("observer pending directory must not be group- or world-writable")
	}
	if info.Mode()&os.ModeSetgid == 0 {
		return nil, fmt.Errorf("observer pending directory must have the setgid bit for group-readable receipts")
	}
	if err := validateHermesObserverOwnership(rootInfo, info); err != nil {
		return nil, err
	}
	pending, err := root.OpenRoot("pending")
	if err != nil {
		return nil, fmt.Errorf("open observer pending directory: %w", err)
	}
	closeRoot = false
	_ = root.Close()
	o := &hermesObserver{
		pending:      pending,
		pendingPath:  filepath.Join(dir, "pending"),
		now:          time.Now,
		random:       rand.Reader,
		maxPending:   hermesObserverMaxPending,
		queue:        make(chan hermesObserverEvent, hermesObserverQueueCapacity),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
		createdTasks: make(map[string]struct{}),
	}
	o.writeReceipt = o.writeEvent
	go o.run()
	return o, nil
}

func (o *hermesObserver) close() {
	if o == nil {
		return
	}
	o.closeOnce.Do(func() {
		close(o.stop)
		select {
		case <-o.done:
			o.closePending()
		case <-time.After(hermesObserverShutdownWait):
			// A mounted filesystem can hang indefinitely. Do not close the Root
			// underneath its lone writer; let it close itself when the I/O returns.
			go func() {
				<-o.done
				o.closePending()
			}()
		}
	})
}

func (o *hermesObserver) closePending() {
	o.closePendingOnce.Do(func() {
		if o.pending != nil {
			_ = o.pending.Close()
		}
	})
}

func (o *hermesObserver) run() {
	defer close(o.done)
	for {
		select {
		case <-o.stop:
			return
		case event := <-o.queue:
			if o.writeReceipt != nil {
				_ = o.writeReceipt(event)
			}
		}
	}
}

// emitActivity records only an already-persisted activity. Engine.emitActivity
// calls this after AppendActivity succeeds and assigns r.ID, so a failed DB write
// can never produce a receipt that falsely claims ARTEX observed the activity.
func (o *hermesObserver) emitActivity(t *Task, a db.Activity) {
	if o == nil || t == nil || a.ID <= 0 {
		return
	}
	at := a.CreatedAt
	if at.IsZero() {
		at = o.now()
	}
	o.enqueueEvent(newHermesActivityEvent(t, a, at))
}

// emitTaskCreated is a metadata-only lifecycle marker. It intentionally does
// not include task description/goal: an event proving the row exists needs only
// ARTEX's opaque task/exploration identifiers and a timestamp.
func (o *hermesObserver) emitTaskCreated(t *Task) {
	if o == nil || t == nil || strings.TrimSpace(t.ID) == "" {
		return
	}
	event := newHermesTaskCreatedEvent(t, o.now())
	o.createdMu.Lock()
	defer o.createdMu.Unlock()
	if _, loaded := o.createdTasks[t.ID]; loaded {
		return
	}
	// Mark only after the non-blocking admission succeeds. A full queue must
	// not permanently suppress a lifecycle event that a later task operation
	// can safely retry.
	if o.enqueueEvent(event) {
		o.createdTasks[t.ID] = struct{}{}
	}
}

// enqueueEvent is deliberately non-blocking. It is called after ARTEX has
// persisted an activity, but never performs a stat, random read, directory scan,
// fsync, or write on that execution path. Queue saturation drops telemetry only.
func (o *hermesObserver) enqueueEvent(event hermesObserverEvent) bool {
	if o == nil || o.pending == nil || event.kind == "" {
		return false
	}
	select {
	case <-o.stop:
		return false
	default:
	}
	select {
	case o.queue <- event:
		return true
	default:
		o.dropped.Add(1)
		return false
	}
}

// writeEvent publishes a 0640 JSON receipt by write+fsync+rename. It is called
// only by run's single writer goroutine. At on-disk capacity, it evicts the
// oldest pending receipt before adding a newer one; this best-effort observer
// never stalls ARTEX waiting for host delivery.
func (o *hermesObserver) writeEvent(event hermesObserverEvent) bool {
	if o == nil || o.pending == nil {
		return false
	}
	at := event.at
	if at.IsZero() {
		at = o.now()
	}
	id, ok := o.nextID(at)
	if !ok {
		return false
	}
	message := event.message(id, at)
	if message == "" {
		return false
	}
	r := hermesObserverReceipt{
		Version:   1,
		ID:        id,
		CreatedAt: at.UTC().Format(time.RFC3339Nano),
		Kind:      event.kind,
		Message:   message,
	}
	body, err := json.Marshal(r)
	if err != nil {
		return false
	}
	body = append(body, '\n')

	if !o.makeCapacity() {
		return false
	}

	tmpName := "." + id + ".tmp"
	tmp, err := o.pending.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	defer func() { _ = o.pending.Remove(tmpName) }() // no-op after successful rename
	// File.Chmod operates on the already-open descriptor. The parent is
	// deployment-owned/setgid, so the pre-provisioned observer group retains
	// read-only access without any root path-based Chown/Chmod operation.
	if err := tmp.Chmod(0o640); err != nil {
		_ = tmp.Close()
		return false
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return false
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false
	}
	if err := tmp.Close(); err != nil {
		return false
	}
	if err := o.pending.Rename(tmpName, id+".json"); err != nil {
		return false
	}
	// Best effort: durable file data matters most, and directory fsync is not
	// supported on every ARTEX platform. A failure is intentionally non-fatal.
	if dir, err := o.pending.Open("."); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return true
}

func (o *hermesObserver) nextID(at time.Time) (string, bool) {
	var entropy [16]byte
	if _, err := io.ReadFull(o.random, entropy[:]); err != nil {
		return "", false
	}
	return fmt.Sprintf("event-%020d-%s", at.UTC().UnixNano(), hex.EncodeToString(entropy[:])), true
}

// makeCapacity bounds the producer-owned pending directory. It evicts
// by filename because generated event IDs have a fixed-width UTC Unix-nanosecond
// prefix. This is an intentionally lossy observer queue: newer live state is
// more useful than stopping ARTEX because an unattended sender stopped.
func (o *hermesObserver) makeCapacity() bool {
	if o.maxPending <= 0 {
		return false
	}
	dir, err := o.pending.Open(".")
	if err != nil {
		return false
	}
	entries, readErr := dir.ReadDir(-1)
	_ = dir.Close()
	if readErr != nil {
		return false
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "event-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		info, err := o.pending.Lstat(name)
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
		files = append(files, name)
	}
	sort.Strings(files)
	for len(files) >= o.maxPending {
		if err := o.pending.Remove(files[0]); err != nil {
			return false
		}
		files = files[1:]
	}
	return true
}

func newHermesActivityEvent(t *Task, a db.Activity, at time.Time) hermesObserverEvent {
	nodeID := "-"
	if a.NodeID != nil {
		nodeID = hermesNonnegativeID(*a.NodeID)
	}
	return hermesObserverEvent{
		kind:          "activity.persisted",
		at:            at,
		taskID:        hermesTaskID(t.ID),
		explorationID: hermesNonnegativeID(t.ExpID),
		activityID:    hermesNonnegativeID(a.ID),
		nodeID:        nodeID,
		activityKind:  hermesActivityKind(a.Kind),
		workerRole:    hermesWorkerRole(a.Worker),
		toolPresent:   strings.TrimSpace(a.Tool) != "",
		isError:       a.IsError,
	}
}

func newHermesTaskCreatedEvent(t *Task, at time.Time) hermesObserverEvent {
	return hermesObserverEvent{
		kind:          "task.created",
		at:            at,
		taskID:        hermesTaskID(t.ID),
		explorationID: hermesNonnegativeID(t.ExpID),
	}
}

// message is deliberately a single fixed-order line. It carries routing-safe
// lifecycle metadata only; it does not derive any field from raw Summary,
// Detail, Metadata, prompt, target, traffic, or artifact content.
func (e hermesObserverEvent) message(eventID string, at time.Time) string {
	switch e.kind {
	case "activity.persisted":
		return strings.Join([]string{
			"timestamp=" + at.UTC().Format(time.RFC3339Nano),
			"event_id=" + eventID,
			"event=activity.persisted",
			"task_id=" + e.taskID,
			"exploration_id=" + e.explorationID,
			"activity_id=" + e.activityID,
			"node_id=" + e.nodeID,
			"kind=" + e.activityKind,
			"worker_role=" + e.workerRole,
			"tool_present=" + strconv.FormatBool(e.toolPresent),
			"is_error=" + strconv.FormatBool(e.isError),
		}, " ")
	case "task.created":
		return strings.Join([]string{
			"timestamp=" + at.UTC().Format(time.RFC3339Nano),
			"event_id=" + eventID,
			"event=task.created",
			"task_id=" + e.taskID,
			"exploration_id=" + e.explorationID,
		}, " ")
	default:
		return ""
	}
}

// hermesActivityMessage and hermesTaskCreatedMessage remain small pure helpers
// for focused contract tests. Production emission snapshots the same sanitized
// event before putting it on the bounded queue.
func hermesActivityMessage(eventID string, t *Task, a db.Activity, at time.Time) string {
	return newHermesActivityEvent(t, a, at).message(eventID, at)
}

func hermesTaskCreatedMessage(eventID string, t *Task, at time.Time) string {
	return newHermesTaskCreatedEvent(t, at).message(eventID, at)
}

// hermesActivityKind retains only a fixed activity vocabulary. A plugin or
// imported activity can carry arbitrary Kind text, so generic safe characters
// alone are not enough to treat it as exportable metadata.
func hermesActivityKind(value string) string {
	switch strings.TrimSpace(value) {
	case "round", "text", "tool_use", "tool_result", "result", "usage", "llm_switch", "system", "user", "thinking":
		return strings.TrimSpace(value)
	default:
		return "-"
	}
}

// hermesWorkerRole reduces a potentially user-configured worker key to one of
// the built-in role classes. It must not turn a custom agent name into an
// external content channel.
func hermesWorkerRole(value string) string {
	switch strings.TrimSpace(value) {
	case "planner":
		return "planner"
	case "mainagent":
		return "mainagent"
	case "system":
		return "system"
	case "user":
		return "user"
	}
	if strings.HasPrefix(strings.TrimSpace(value), "worker") || strings.HasPrefix(strings.TrimSpace(value), "work") {
		return "worker"
	}
	return "-"
}

// hermesTaskID accepts only the numeric database identity generated by Manager.
// A Task.ID is stored as a string for routing compatibility, so it must not
// become a generic text channel in an otherwise metadata-only receipt.
func hermesTaskID(value string) string {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id < 0 {
		return "-"
	}
	return strconv.FormatInt(id, 10)
}

// hermesNonnegativeID preserves machine identifiers without admitting a
// negative sentinel into the sender grammar. Database-generated IDs are
// non-negative; an impossible negative value is reduced to the harmless zero.
func hermesNonnegativeID(id int64) string {
	if id < 0 {
		return "0"
	}
	return strconv.FormatInt(id, 10)
}
