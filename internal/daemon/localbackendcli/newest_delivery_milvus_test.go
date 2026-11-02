//go:build milvusintegration

package localbackendcli_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

const (
	newestWorkspace        = "/newest/delivery"
	newestOlderPrefix      = "newest-older-"
	newestWaitingSession   = "newest-a-waiting"
	newestFreshPrefix      = "newest-c-fresh-"
	newestExistingSession  = "newest-y-existing"
	newestStartupSession   = "newest-z-startup"
	newestChangedSession   = "newest-z-changed"
	newestActiveSession    = "newest-z-active"
	newestIdleSession      = "newest-z-idle"
	newestActiveDelay      = 10 * time.Second
	newestRestoredSends    = 3
	newestIndexRefreshWait = 5 * time.Second
	newestOlderChats       = 10
	newestLongBacklog      = 16
	newestShortBacklog     = 5
	newestLaterChats       = 3
	newestOlderMessages    = 8
	newestWaitingMessages  = 2
	newestChangedMessages  = 3
	newestBallastBytes     = (8 << 20) + 1024
	newestBallastLine      = 1 << 20
	newestRetimedAge       = 5*time.Hour + 30*time.Minute
	newestFirstAge         = 6 * time.Hour
	newestOlderAge         = 3 * time.Hour
	newestLongWaitAge      = time.Hour
	newestRetimedWaitAge   = 50 * time.Minute
	newestWaitingAge       = 20 * time.Minute
	newestFreshAge         = 90 * time.Second
	newestLaterAge         = 80 * time.Second
	newestFreshPending     = 2
	newestFreshPerGroup    = 8
	newestWaitingChats     = 2
	newestWaitLimit        = 20 * time.Minute
	newestPollDelay        = 100 * time.Millisecond
	newestCountField       = "count(*)"
	newestCheckpointChars  = 32
	newestSearchLimit      = 5
	newestPassEvent        = "daemon.conversation_semantic_sync.pass_completed"
	newestPinnedEvent      = "daemon.conversation_semantic_sync.unchanged_pinned"
	newestIndexCacheName   = "conversation-index.json"
	newestUpsertEvent      = "conversation.vectorsearch.upsert_completed"
	newestLongInterval     = "sync_interval = \"5m\"\nindex_refresh_interval = \"1s\"\n"
	newestMetaField        = `"isMeta":true,`

	newestCodexProvider     = "codex"
	newestRolloutDate       = "2026-05-02"
	newestRolloutStamp      = "2026-05-02T10-09-00"
	newestRecheckIDFormat   = "019de9cc-3a00-7010-bd9f-a6ee7155%04d"
	newestRolloutNewID      = "019de9cc-3a00-7010-bd9f-ffffffffffff"
	newestRolloutSecondID   = "019de9cc-3a00-7010-bd9f-fffffffffffe"
	newestRechecks          = 12
	newestRecheckMessages   = 4
	newestIncompressibleRaw = 9 << 20
	newestRecheckLead       = 1
)

type newestPassRecord struct {
	Time                time.Time `json:"time"`
	Message             string    `json:"msg"`
	SentConversationIDs []string  `json:"sent_conversation_ids"`
	FirstListedIDs      []string  `json:"first_listed_conversation_ids"`
	UnchangedPinned     int       `json:"unchanged_pinned"`
	PinnedIDs           []string  `json:"conversation_ids"`
	PolicySkipped       int       `json:"policy_skipped"`
	Failed              int       `json:"failed"`
	WrittenChats        []string  `json:"written_conversations"`
}

// newestArrivals tracks fresh chats as the test installs them in batches.
type newestArrivals struct {
	mu       sync.Mutex
	sessions []string
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

type newestFixture struct {
	t              *testing.T
	home           string
	daemon         *localProofDaemon
	client         *milvusclient.Client
	collectionName string
	deadline       time.Time
}

func newestConversationID(session string) string {
	return localProofProviderClaude + ":" + session
}

func newestToken(session string, position int) string {
	return fmt.Sprintf("zq%stoken%04dx", strings.ReplaceAll(session, "-", ""), position)
}

func newestUUID(session string, position int) string {
	digest := sha256.Sum256(fmt.Appendf(nil, "%s-%d", session, position))
	encoded := hex.EncodeToString(digest[:])
	return encoded[:8] + "-" + encoded[8:12] + "-4" + encoded[13:16] + "-8" + encoded[17:20] + "-" + encoded[20:32]
}

func newestMessageLine(session string, position int, extraField string, start time.Time) string {
	text := fmt.Sprintf("newest delivery record %s harbor survey step %04d", newestToken(session, position), position)
	return fmt.Sprintf(`{"type":"user",%s"message":{"role":"user","content":%q},"sessionId":%q,"cwd":%q,"uuid":%q,"timestamp":%q}`+"\n",
		extraField, text, session, newestWorkspace, newestUUID(session, position), start.Add(time.Duration(position)*time.Second).Format(time.RFC3339))
}

// newestTranscript writes user messages before ballast records. The Claude header
// scan reads the first lines and limits each line to 4 MiB.
func newestTranscript(session string, messages int, ballastBytes int, start time.Time) string {
	var builder strings.Builder
	for position := range messages {
		builder.WriteString(newestMessageLine(session, position, "", start))
	}
	line := fmt.Sprintf(`{"type":"queue-operation","operation":"enqueue","sessionId":%q,"timestamp":%q,"content":%q}`+"\n",
		session, start.Format(time.RFC3339), strings.Repeat("b", newestBallastLine))
	for written := 0; written < ballastBytes; written += len(line) {
		builder.WriteString(line)
	}
	return builder.String()
}

func newestProjectDirectory(home string) string {
	return filepath.Join(home, ".claude", "projects", strings.ReplaceAll(newestWorkspace, "/", "-"))
}

func writeNewestFile(path string, body []byte, modified time.Time) error {
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write transcript file: %w", err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		return fmt.Errorf("set transcript access and modification times: %w", err)
	}
	return nil
}

// installNewestTranscript writes the transcript outside the scanned folder and
// sets its modification time before renaming it into that folder.
func installNewestTranscript(home string, session string, body string, modified time.Time) error {
	directory := newestProjectDirectory(home)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create the transcript folder: %w", err)
	}
	temporary := filepath.Join(home, session+".tmp")
	if err := writeNewestFile(temporary, []byte(body), modified); err != nil {
		return err
	}
	if err := os.Rename(temporary, filepath.Join(directory, session+".jsonl")); err != nil {
		return fmt.Errorf("rename transcript into the scanned folder: %w", err)
	}
	return nil
}

func newestStatusCounter(t *testing.T, daemon *localProofDaemon, name string) any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), localProofCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, daemon.binaryPath, "status", "--output-format", "json")
	command.Env = daemon.environment
	output, err := command.Output()
	if err != nil {
		return nil
	}
	var status struct {
		Counters map[string]struct {
			Value any `json:"value"`
		} `json:"counters"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		t.Fatalf("The test could not decode daemon status: %v", err)
	}
	return status.Counters[name].Value
}

func (f *newestFixture) rowsFor(conversationID string) (int64, error) {
	filter := fmt.Sprintf("conversationId == %q", conversationID)
	result, err := f.client.Query(context.Background(), milvusclient.NewQueryOption(f.collectionName).
		WithFilter(filter).WithOutputFields(newestCountField).WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		return 0, fmt.Errorf("query Milvus row count: %w", err)
	}
	return result.GetColumn(newestCountField).GetAsInt64(0)
}

func (f *newestFixture) rows(session string) (int64, error) {
	return f.rowsFor(newestConversationID(session))
}

func (f *newestFixture) checkpointDirectory() string {
	return filepath.Join(f.daemon.roots.State, "clyde", "conversation-semantic", "checkpoints", f.collectionName)
}

func (f *newestFixture) checkpointFor(conversationID string) (string, bool) {
	digest := sha256.Sum256([]byte(conversationID))
	name := hex.EncodeToString(digest[:])[:newestCheckpointChars] + ".json"
	content, err := os.ReadFile(filepath.Join(f.checkpointDirectory(), name))
	if err != nil {
		return "", false
	}
	return string(content), true
}

func (f *newestFixture) checkpointPresent(session string) bool {
	_, present := f.checkpointFor(newestConversationID(session))
	return present
}

func (f *newestFixture) finished(sessions []string) int {
	finished := 0
	for _, session := range sessions {
		if f.checkpointPresent(session) {
			finished++
		}
	}
	return finished
}

// waitFor polls check until check returns true or the fixture deadline passes.
func (f *newestFixture) waitFor(description string, check func() bool) {
	f.t.Helper()
	for !check() {
		if time.Now().After(f.deadline) {
			f.t.Fatalf("The test did not observe %s before the fixture deadline. Daemon stderr: %s", description, f.daemon.stderr.String())
		}
		time.Sleep(newestPollDelay)
	}
}

func (f *newestFixture) waitForCollection() {
	f.t.Helper()
	f.waitFor("a collection name in daemon status", func() bool {
		name, isText := newestStatusCounter(f.t, f.daemon, "semantic_settings.collection_name").(string)
		f.collectionName = name
		return isText && name != ""
	})
}

func (f *newestFixture) waitForRows(session string, want int64) {
	f.t.Helper()
	f.waitFor(fmt.Sprintf("exactly %d Milvus rows for chat %s", want, session), func() bool {
		count, err := f.rows(session)
		return err == nil && count == want
	})
}

// requireExactMatch checks that CLI and MCP search results include the requested
// conversation ID and message index.
func (f *newestFixture) requireExactMatch(conversationID string, query string, position int) {
	f.t.Helper()
	for _, surface := range f.daemon.surfaces() {
		result := mustLocalProofSearch(f.t, surface, localProofSearchRequest{Query: query, Limit: newestSearchLimit, ConversationID: conversationID})
		found := slices.ContainsFunc(result.Matches, func(match localProofMatch) bool {
			return match.Conversation.ID == conversationID && match.MessageIndex == position
		})
		if !found {
			f.t.Fatalf("%s did not return conversation %s at message index %d. Search matches: %+v", surface.name, conversationID, position, result.Matches)
		}
	}
}

func (f *newestFixture) requireExactSearch(session string, position int) {
	f.t.Helper()
	f.requireExactMatch(newestConversationID(session), newestToken(session, position), position)
}

// passRecords returns sync pass log records from both state roots in time order.
func (f *newestFixture) passRecords() []newestPassRecord {
	f.t.Helper()
	return f.logRecords(newestPassEvent)
}

// logRecords reads records for the requested event from both daemon state roots
// and sorts them by time.
func (f *newestFixture) logRecords(event string) []newestPassRecord {
	f.t.Helper()
	records := []newestPassRecord{}
	for _, line := range daemonLogLines(f.t, f.home, f.daemon, event) {
		var record newestPassRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			f.t.Fatalf("The test could not decode a sync pass log record: %v.", err)
		}
		if record.Message == event {
			records = append(records, record)
		}
	}
	sort.SliceStable(records, func(left, right int) bool { return records[left].Time.Before(records[right].Time) })
	return records
}

func newestPassesListing(records []newestPassRecord, conversationID string) []int {
	passes := []int{}
	for position, record := range records {
		if slices.Contains(record.FirstListedIDs, conversationID) {
			passes = append(passes, position)
		}
	}
	return passes
}

func newestPassesSending(records []newestPassRecord, conversationID string) []int {
	passes := []int{}
	for position, record := range records {
		if slices.Contains(record.SentConversationIDs, conversationID) {
			passes = append(passes, position)
		}
	}
	return passes
}

// newestLastPass returns the latest pass that find reports for any session.
// It returns -1 if find does not report a pass for any session.
func newestLastPass(records []newestPassRecord, sessions []string, find func([]newestPassRecord, string) []int) int {
	last := -1
	for _, session := range sessions {
		for _, pass := range find(records, newestConversationID(session)) {
			last = max(last, pass)
		}
	}
	return last
}

// newestFirstSendOfAny returns the first pass that sends a requested conversation.
// It returns -1 if every pass omits those conversations.
func newestFirstSendOfAny(records []newestPassRecord, conversationIDs []string) int {
	for position, record := range records {
		for _, conversationID := range conversationIDs {
			if slices.Contains(record.SentConversationIDs, conversationID) {
				return position
			}
		}
	}
	return -1
}

// newestSentBefore counts sessions that the worker sent before the specified pass.
func newestSentBefore(records []newestPassRecord, sessions []string, pass int) int {
	count := 0
	for _, session := range sessions {
		sends := newestPassesSending(records, newestConversationID(session))
		if len(sends) > 0 && sends[0] < pass {
			count++
		}
	}
	return count
}

func (f *newestFixture) waitForNoNeededChats() {
	f.t.Helper()
	f.waitFor("an empty needed listing", func() bool {
		needed, isNumber := newestStatusCounter(f.t, f.daemon, "semantic_freshness.needed").(float64)
		return isNumber && needed == 0
	})
}

func (f *newestFixture) firstListedCount(conversationID string) int {
	f.t.Helper()
	return len(newestPassesListing(f.passRecords(), conversationID))
}

func (f *newestFixture) sentCount(conversationID string) int {
	f.t.Helper()
	return len(newestPassesSending(f.passRecords(), conversationID))
}

func (f *newestFixture) waitForFirstListing(conversationID string, count int) {
	f.t.Helper()
	f.waitFor(fmt.Sprintf("at least %d sync passes that first list %s", count, conversationID), func() bool {
		return f.firstListedCount(conversationID) >= count
	})
}

// newestProgress tracks completed chats in each group and fails the test if fresh
// chats continue to finish beyond the limit without progress in a waiting group.
type newestProgress struct {
	fixture  *newestFixture
	arrivals *newestArrivals
	groups   map[string][]string
	finished map[string]int
	freshAt  map[string]int
}

func newNewestProgress(fixture *newestFixture, arrivals *newestArrivals, groups map[string][]string) *newestProgress {
	progress := &newestProgress{fixture: fixture, arrivals: arrivals, groups: groups, finished: map[string]int{}, freshAt: map[string]int{}}
	freshFinished := fixture.finished(arrivals.list())
	for name, sessions := range groups {
		progress.finished[name] = fixture.finished(sessions)
		progress.freshAt[name] = freshFinished
	}
	return progress
}

// check returns true when every chat in every group has a checkpoint.
func (p *newestProgress) check() bool {
	p.fixture.t.Helper()
	freshFinished := p.fixture.finished(p.arrivals.list())
	complete := true
	for name, sessions := range p.groups {
		finished := p.fixture.finished(sessions)
		if finished > p.finished[name] {
			p.finished[name] = finished
			p.freshAt[name] = freshFinished
		}
		if finished < len(sessions) {
			complete = false
			if freshFinished-p.freshAt[name] > newestFreshPerGroup {
				p.fixture.t.Fatalf("%d fresh chats finished while group %s remained at %d completed chats out of %d.", freshFinished-p.freshAt[name], name, finished, len(sessions))
			}
		}
	}
	return complete
}

// newestCohortChat stores a transcript's session ID, content, and modification time.
type newestCohortChat struct {
	session  string
	body     string
	modified time.Time
}

// installNewestCohort renames a folder of transcripts into the scanned tree and
// returns its path. The next scan reads the transcripts as one cohort.
func installNewestCohort(t *testing.T, home string, chats []newestCohortChat) string {
	t.Helper()
	temporary := filepath.Join(home, "cohort-folder")
	if err := os.MkdirAll(temporary, 0o700); err != nil {
		t.Fatalf("The test could not create the cohort folder: %v", err)
	}
	for _, chat := range chats {
		if err := writeNewestFile(filepath.Join(temporary, chat.session+".jsonl"), []byte(chat.body), chat.modified); err != nil {
			t.Fatalf("The test could not write the cohort transcript: %v", err)
		}
	}
	destination := newestProjectDirectory(home) + "-cohort"
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatalf("The test could not create the projects folder: %v", err)
	}
	if err := os.Rename(temporary, destination); err != nil {
		t.Fatalf("The test could not rename the cohort folder into the scanned tree: %v", err)
	}
	return destination
}

// startNewestActiveChat appends a message at each interval. It returns a function
// that stops the writes and reports the final message count.
func startNewestActiveChat(fixture *newestFixture, path string, session string, messages int, start time.Time) func() int {
	stop := make(chan struct{})
	finished := make(chan int, 1)
	go func() {
		count := messages
		defer func() { finished <- count }()
		for {
			select {
			case <-stop:
				return
			case <-time.After(newestActiveDelay):
			}
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				fixture.t.Errorf("The test could not open the active transcript for appending: %v", err)
				return
			}
			_, writeErr := file.WriteString(newestMessageLine(session, count, "", start))
			closeErr := file.Close()
			if err := errors.Join(writeErr, closeErr); err != nil {
				fixture.t.Errorf("The test could not append to or close the active transcript: %v", err)
				return
			}
			count++
		}
	}()
	halt := sync.OnceValue(func() int {
		close(stop)
		return <-finished
	})
	fixture.t.Cleanup(func() { halt() })
	return halt
}

func (f *newestFixture) requireOlderRows(older []string) {
	f.t.Helper()
	for _, session := range older {
		count, err := f.rows(session)
		if err != nil {
			f.t.Fatalf("The test could not count Milvus rows for an older chat: %v", err)
		}
		if count != newestOlderMessages {
			f.t.Fatalf("Older chat %s has %d Milvus rows; the test expected %d.", session, count, newestOlderMessages)
		}
	}
}

func newestOlderSessions(count int) []string {
	older := make([]string, 0, count)
	for position := range count {
		older = append(older, fmt.Sprintf("%s%02d", newestOlderPrefix, position))
	}
	return older
}

func installNewestOlder(t *testing.T, home string, count int) []string {
	t.Helper()
	older := newestOlderSessions(count)
	modified := time.Now().Add(-newestOlderAge)
	for _, session := range older {
		if err := installNewestTranscript(home, session, newestTranscript(session, newestOlderMessages, newestBallastBytes, modified), modified); err != nil {
			t.Fatalf("The test could not install an older transcript: %v", err)
		}
	}
	return older
}

func (f *newestFixture) startDaemon(settings string) {
	f.t.Helper()
	settingsPath := filepath.Join(f.t.TempDir(), "semantic-settings.toml")
	writeLocalProofFile(f.t, settingsPath, settings)
	f.daemon = startLocalProofDaemon(f.t, buildClydeCLI(f.t), f.home, "--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath)
	f.waitForCollection()
}

func newestEmbeddingBaseURL() string {
	baseURL := os.Getenv(deletedSourceEmbeddingEnv)
	if baseURL == "" {
		return deletedSourceEmbeddingDefault
	}
	return baseURL
}

// startDaemonWithCachedIndex builds the index without ingestion, then starts an
// ingesting daemon with the same root. The first sync pass reads the cached index
// before the long sync interval elapses.
func (f *newestFixture) startDaemonWithCachedIndex(settings string, session string) {
	f.t.Helper()
	settingsPath := filepath.Join(f.t.TempDir(), "semantic-settings.toml")
	writeLocalProofFile(f.t, settingsPath, settings)
	binaryPath := buildClydeCLI(f.t)
	f.daemon = startLocalProofDaemon(f.t, binaryPath, f.home, "--keep", "--semantic-settings", settingsPath)
	base := f.daemon.roots.Base
	f.t.Cleanup(func() { _ = os.RemoveAll(base) })
	cachePath := filepath.Join(f.daemon.roots.Cache, "clyde", newestIndexCacheName)
	f.waitFor("the requested chat in the cached conversation index", func() bool {
		content, err := os.ReadFile(cachePath)
		return err == nil && strings.Contains(string(content), session)
	})
	f.daemon.stop()
	f.daemon = startLocalProofDaemon(f.t, binaryPath, f.home, "--root", base, "--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath)
	f.waitForCollection()
}

func newestMilvusSettings(t *testing.T, intervals string) (*newestFixture, string) {
	t.Helper()
	return newestMilvusSettingsFor(t, intervals, newestEmbeddingBaseURL())
}

func newestMilvusSettingsFor(t *testing.T, intervals string, baseURL string) (*newestFixture, string) {
	t.Helper()
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	client, err := milvusclient.New(context.Background(), &milvusclient.ClientConfig{Address: address})
	if err != nil {
		t.Fatalf("The test could not connect to Milvus: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	fixture := &newestFixture{t: t, home: t.TempDir(), daemon: nil, client: client, collectionName: "", deadline: time.Now().Add(newestWaitLimit)}
	return fixture, intervals + fmt.Sprintf(withheldSettings, address, baseURL, deletedSourceEmbeddingModel, deletedSourceEmbeddingDim)
}

// startNewestArrivals installs fresh chats whenever fewer than newestFreshPending
// fresh chats await checkpoints.
func startNewestArrivals(fixture *newestFixture) *newestArrivals {
	arrivals := &newestArrivals{mu: sync.Mutex{}, sessions: nil, stop: make(chan struct{}), done: make(chan struct{}), stopOnce: sync.Once{}}
	go func() {
		defer close(arrivals.done)
		for {
			select {
			case <-arrivals.stop:
				return
			case <-time.After(newestPollDelay):
			}
			sessions := arrivals.list()
			if len(sessions)-fixture.finished(sessions) >= newestFreshPending {
				continue
			}
			session := fmt.Sprintf("%s%04d", newestFreshPrefix, len(sessions))
			modified := time.Now().Add(-newestFreshAge)
			body := newestTranscript(session, 1, newestBallastBytes, modified)
			if err := arrivals.install(fixture.home, session, body, modified); err != nil {
				fixture.t.Errorf("The test could not install a fresh transcript: %v", err)
				return
			}
		}
	}()
	fixture.t.Cleanup(arrivals.halt)
	return arrivals
}

// install has the arrivals lock while it installs the transcript in the scanned
// folder and appends the session ID.
func (a *newestArrivals) install(home string, session string, body string, modified time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := installNewestTranscript(home, session, body, modified); err != nil {
		return err
	}
	a.sessions = append(a.sessions, session)
	return nil
}

func (a *newestArrivals) list() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.sessions)
}

func (a *newestArrivals) halt() {
	a.stopOnce.Do(func() { close(a.stop) })
	<-a.done
}

// TestMilvusFeederServesWaitingChatWithoutStarvingOlderChats checks that waiting
// chats and older chats make progress during ongoing arrivals in the sandbox daemon.
func TestMilvusFeederServesWaitingChatWithoutStarvingOlderChats(t *testing.T) {
	gate := startNewestEmbeddingGate(t)
	fixture, settings := newestMilvusSettingsFor(t, newestLongInterval, gate.baseURL)
	older := installNewestOlder(t, fixture.home, newestOlderChats)
	// The closed gate blocks the first upsert's embedding request. The daemon lists
	// the waiting and fresh chats before the worker completes any older chat.
	gate.close()
	fixture.startDaemonWithCachedIndex(settings, older[0])
	fixture.waitFor("an embedding request waiting for the test gate to open", func() bool { return gate.blocked.Load() > 0 })
	arrivals := startNewestArrivals(fixture)

	// The cohort contains waiting chats with file times older than each fresh chat.
	waiting := make([]string, 0, newestWaitingChats)
	waitingChats := make([]newestCohortChat, 0, newestWaitingChats)
	waitingModified := time.Now().Add(-newestWaitingAge)
	for position := range newestWaitingChats {
		session := fmt.Sprintf("%s-%02d", newestWaitingSession, position)
		waiting = append(waiting, session)
		waitingChats = append(waitingChats, newestCohortChat{
			session:  session,
			body:     newestTranscript(session, newestWaitingMessages, newestBallastBytes, waitingModified),
			modified: waitingModified,
		})
	}
	installNewestCohort(t, fixture.home, waitingChats)
	time.Sleep(newestIndexRefreshWait)
	gate.open()
	for _, session := range waiting {
		fixture.waitForFirstListing(newestConversationID(session), 1)
	}

	progress := newNewestProgress(fixture, arrivals, map[string][]string{"older": older, "waiting": waiting})
	fixture.waitFor("checkpoints for all waiting and older chats", progress.check)
	installedAtFinish := len(arrivals.list())
	arrivals.halt()

	records := fixture.passRecords()
	listedPass := newestLastPass(records, waiting, newestPassesListing)
	olderAtListing := newestSentBefore(records, older, listedPass)
	cohortStart := newestFirstSendOfAny(records, records[listedPass].FirstListedIDs)
	olderAtCohortStart := newestSentBefore(records, older, cohortStart)
	t.Logf("The worker sent %d older chats before the waiting listing and %d before the waiting cohort's first delivery out of %d older chats. The test installed %d fresh chats.", olderAtListing, olderAtCohortStart, newestOlderChats, installedAtFinish)
	if olderAtListing == newestOlderChats {
		t.Fatalf("The worker sent all %d older chats before the daemon listed the waiting chats.", newestOlderChats)
	}
	if cohortStart < 0 || olderAtCohortStart == newestOlderChats {
		t.Fatalf("The waiting cohort's first delivery pass is %d for a backlog of %d older chats; the test expected a delivery before the backlog finished.", cohortStart, newestOlderChats)
	}

	fixture.requireOlderRows(older)
	fixture.requireExactSearch(older[len(older)-1], newestOlderMessages-1)
	for _, session := range waiting {
		count, err := fixture.rows(session)
		if err != nil {
			t.Fatalf("The test could not count Milvus rows for a waiting chat: %v", err)
		}
		if count != newestWaitingMessages {
			t.Fatalf("Waiting chat %s has %d Milvus rows; the test expected %d.", session, count, newestWaitingMessages)
		}
		fixture.requireExactSearch(session, newestWaitingMessages-1)
	}
	fresh := arrivals.list()
	fixture.waitFor("checkpoints for all fresh chats", func() bool { return fixture.finished(fresh) == len(fresh) })
	fixture.requireExactSearch(fresh[len(fresh)-1], 0)

	for _, session := range older {
		if sent := fixture.sentCount(newestConversationID(session)); sent != 1 {
			t.Fatalf("The worker sent older chat %s %d times; the test expected one delivery.", session, sent)
		}
	}
}

// TestMilvusFeederServesIdleChatBehindActiveChat checks that an idle chat and older
// chats make progress while a newer chat in the cohort grows and arrivals continue.
func TestMilvusFeederServesIdleChatBehindActiveChat(t *testing.T) {
	fixture, settings := newestMilvusSettings(t, sandboxFastIntervals)
	older := installNewestOlder(t, fixture.home, newestLongBacklog)
	fixture.startDaemon(settings)
	fixture.waitFor("the first older chat", func() bool { return fixture.finished(older) > 0 })
	arrivals := startNewestArrivals(fixture)

	activeModified := time.Now()
	idleModified := time.Now().Add(-newestWaitingAge)
	destination := installNewestCohort(t, fixture.home, []newestCohortChat{
		{session: newestActiveSession, body: newestTranscript(newestActiveSession, newestWaitingMessages, 0, activeModified), modified: activeModified},
		{session: newestIdleSession, body: newestTranscript(newestIdleSession, newestWaitingMessages, 0, idleModified), modified: idleModified},
	})
	activePath := filepath.Join(destination, newestActiveSession+".jsonl")
	stopActive := startNewestActiveChat(fixture, activePath, newestActiveSession, newestWaitingMessages, activeModified)

	activeID := newestConversationID(newestActiveSession)
	idleID := newestConversationID(newestIdleSession)
	fixture.waitForFirstListing(idleID, 1)
	sameCohort := slices.ContainsFunc(fixture.passRecords(), func(record newestPassRecord) bool {
		return slices.Contains(record.FirstListedIDs, activeID) && slices.Contains(record.FirstListedIDs, idleID)
	})
	if !sameCohort {
		t.Fatalf("The daemon did not first list the active chat and the idle chat in the same pass.")
	}

	idleGroup := []string{newestIdleSession}
	progress := newNewestProgress(fixture, arrivals, map[string][]string{"older": older, "idle": idleGroup})
	olderAtIdleFinish := -1
	fixture.waitFor("checkpoints for the idle chat and all older chats", func() bool {
		complete := progress.check()
		if olderAtIdleFinish < 0 && fixture.finished(idleGroup) == len(idleGroup) {
			olderAtIdleFinish = fixture.finished(older)
		}
		return complete
	})
	if olderAtIdleFinish == newestLongBacklog {
		t.Fatalf("The idle chat finished after all %d older chats; the test expected it to finish before the older backlog completed.", newestLongBacklog)
	}
	t.Logf("The idle chat finished after %d of %d older chats had finished.", olderAtIdleFinish, newestLongBacklog)
	if sent := fixture.sentCount(activeID); sent != 0 {
		t.Fatalf("The worker sent the active chat %d times during transcript growth; the test expected zero deliveries.", sent)
	}
	fixture.requireExactSearch(newestIdleSession, newestWaitingMessages-1)

	// The active chat has the same cohort and position after its file stops changing.
	activeMessages := stopActive()
	fixture.waitForRows(newestActiveSession, int64(activeMessages))
	if listed := fixture.firstListedCount(activeID); listed != 1 {
		t.Fatalf("The daemon first listed the active chat %d times; the test expected one listing.", listed)
	}
	fixture.requireExactSearch(newestActiveSession, activeMessages-1)
	arrivals.halt()
	fixture.requireOlderRows(older)
}

// newestEmbeddingGate proxies embedding requests to the real endpoint after the
// test opens the gate.
type newestEmbeddingGate struct {
	mu      sync.Mutex
	release chan struct{}
	blocked atomic.Int64
	baseURL string
}

func startNewestEmbeddingGate(t *testing.T) *newestEmbeddingGate {
	t.Helper()
	target, err := url.Parse(newestEmbeddingBaseURL())
	if err != nil {
		t.Fatalf("The test could not parse the embedding endpoint URL: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: target.Scheme, Host: target.Host})
	gate := &newestEmbeddingGate{mu: sync.Mutex{}, release: nil, blocked: atomic.Int64{}, baseURL: ""}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gate.mu.Lock()
		release := gate.release
		gate.mu.Unlock()
		if release != nil {
			gate.blocked.Add(1)
			select {
			case <-release:
			case <-request.Context().Done():
				return
			}
		}
		proxy.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(gate.open)
	gate.baseURL = server.URL + target.Path
	return gate
}

func (g *newestEmbeddingGate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.release = make(chan struct{})
}

func (g *newestEmbeddingGate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.release != nil {
		close(g.release)
		g.release = nil
	}
}

// TestMilvusFeederRedeliversChatRestoredDuringChangedDelivery checks delivery after
// a chat changes from A to B and back to A while B's embedding request waits.
func TestMilvusFeederRedeliversChatRestoredDuringChangedDelivery(t *testing.T) {
	gate := startNewestEmbeddingGate(t)
	fixture, settings := newestMilvusSettingsFor(t, sandboxFastIntervals, gate.baseURL)
	chatID := newestConversationID(newestChangedSession)
	start := time.Now().Add(-newestFirstAge)
	contentA := newestTranscript(newestChangedSession, newestWaitingMessages, 0, start)
	if err := installNewestTranscript(fixture.home, newestChangedSession, contentA, start); err != nil {
		t.Fatalf("The test could not install content A: %v", err)
	}
	fixture.startDaemon(settings)
	fixture.waitFor("a checkpoint for content A", func() bool { return fixture.checkpointPresent(newestChangedSession) })
	fixture.waitForNoNeededChats()
	checkpointA, _ := fixture.checkpointFor(chatID)

	gate.close()
	contentB := newestTranscript(newestChangedSession, newestChangedMessages, 0, start)
	if err := installNewestTranscript(fixture.home, newestChangedSession, contentB, time.Now().Add(-newestRetimedAge)); err != nil {
		t.Fatalf("The test could not install content B: %v", err)
	}
	fixture.waitFor("an embedding request for content B waiting for the test gate to open", func() bool { return gate.blocked.Load() > 0 })
	if err := installNewestTranscript(fixture.home, newestChangedSession, contentA, time.Now().Add(-newestRetimedWaitAge)); err != nil {
		t.Fatalf("The test could not restore content A: %v", err)
	}
	time.Sleep(newestIndexRefreshWait)
	if checkpoint, _ := fixture.checkpointFor(chatID); checkpoint != checkpointA {
		t.Fatalf("The checkpoint changed before the embedding request finished.")
	}
	gate.open()

	fixture.waitFor("a total of three recorded deliveries for the restored chat", func() bool {
		records := fixture.passRecords()
		if slices.ContainsFunc(records, func(record newestPassRecord) bool { return record.UnchangedPinned > 0 }) {
			t.Fatalf("The worker pinned the restored chat to a fingerprint that the checkpoint does not record.")
		}
		return len(newestPassesSending(records, chatID)) == newestRestoredSends
	})
	fixture.waitForNoNeededChats()
	checkpoint, _ := fixture.checkpointFor(chatID)
	if checkpoint == checkpointA {
		t.Fatalf("The checkpoint still records the first delivery of content A.")
	}
	fixture.requireExactSearch(newestChangedSession, newestWaitingMessages-1)
}

// TestMilvusFeederServesChatWaitingAtStartup checks that a fresh chat from the first
// listing finishes before the worker completes the older backlog.
func TestMilvusFeederServesChatWaitingAtStartup(t *testing.T) {
	fixture, settings := newestMilvusSettings(t, newestLongInterval)
	older := installNewestOlder(t, fixture.home, newestOlderChats)
	startupModified := time.Now().Add(-newestFreshAge)
	if err := installNewestTranscript(fixture.home, newestStartupSession, newestTranscript(newestStartupSession, newestWaitingMessages, 0, startupModified), startupModified); err != nil {
		t.Fatalf("The test could not install the startup transcript: %v", err)
	}
	fixture.startDaemonWithCachedIndex(settings, newestStartupSession)
	arrivals := startNewestArrivals(fixture)

	olderAtStartupChat := -1
	fixture.waitFor("a checkpoint for the startup chat", func() bool {
		finished := fixture.finished(older)
		if fixture.checkpointPresent(newestStartupSession) {
			olderAtStartupChat = finished
			return true
		}
		return false
	})
	if olderAtStartupChat == newestOlderChats {
		t.Fatalf("The startup chat finished after all %d older chats; the test expected it to finish before the older backlog completed.", newestOlderChats)
	}
	fixture.requireExactSearch(newestStartupSession, newestWaitingMessages-1)
	progress := newNewestProgress(fixture, arrivals, map[string][]string{"older": older})
	fixture.waitFor("checkpoints for all older chats", progress.check)
	arrivals.halt()
	fixture.requireOlderRows(older)
}

func newestRolloutBody(threadID string, messages int, ballastBytes int) (string, error) {
	var builder strings.Builder
	fmt.Fprintf(&builder, `{"timestamp":"2026-05-02T17:09:00.000Z","type":"session_meta","payload":{"id":%q,"timestamp":"2026-05-02T17:09:00.000Z","cwd":%q,"originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}`+"\n", threadID, newestWorkspace)
	for position := range messages {
		fmt.Fprintf(&builder, `{"timestamp":"2026-05-02T17:09:%02d.000Z","type":"event_msg","payload":{"type":"user_message","message":%q}}`+"\n", position+1, newestToken(threadID, position)+" compressed recheck harbor survey")
	}
	for written := 0; written < ballastBytes; written += newestBallastLine {
		random := make([]byte, newestBallastLine)
		if _, err := rand.Read(random); err != nil {
			return "", fmt.Errorf("generate random padding bytes for the rollout: %w", err)
		}
		fmt.Fprintf(&builder, `{"timestamp":"2026-05-02T17:09:30.000Z","type":"event_msg","payload":{"type":"token_count","info":%q}}`+"\n", base64.StdEncoding.EncodeToString(random))
	}
	return builder.String(), nil
}

func newestRolloutPath(sessions string, threadID string) string {
	return filepath.Join(sessions, strings.ReplaceAll(newestRolloutDate, "-", "/"), "rollout-"+newestRolloutStamp+"-"+threadID+".jsonl")
}

func newestCodexID(threadID string) string {
	return newestCodexProvider + ":" + threadID
}

// TestMilvusFeederOrdersChatsWithCompressedRechecksAndArrivals checks delivery of
// new and waiting chats during ongoing arrivals and rechecks of compressed rollouts
// with stored rows.
func TestMilvusFeederOrdersChatsWithCompressedRechecksAndArrivals(t *testing.T) {
	fixture, settings := newestMilvusSettings(t, sandboxFastIntervals)
	sessions := filepath.Join(fixture.home, ".codex", "sessions")
	oldModified := time.Now().Add(-newestOlderAge)
	bodies := map[string]string{}
	threadIDs := make([]string, 0, newestRechecks)
	for position := range newestRechecks {
		threadID := fmt.Sprintf(newestRecheckIDFormat, position)
		threadIDs = append(threadIDs, threadID)
		body, err := newestRolloutBody(threadID, newestRecheckMessages, newestIncompressibleRaw)
		if err != nil {
			t.Fatalf("The test could not build the rollout content: %v", err)
		}
		bodies[threadID] = body
		path := newestRolloutPath(sessions, threadID)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("The test could not create the session folder: %v", err)
		}
		if err := writeNewestFile(path, []byte(body), oldModified); err != nil {
			t.Fatalf("The test could not write the rollout file: %v", err)
		}
	}
	existingModified := time.Now().Add(-newestFreshAge)
	if err := installNewestTranscript(fixture.home, newestExistingSession, newestTranscript(newestExistingSession, newestWaitingMessages, 0, existingModified), existingModified); err != nil {
		t.Fatalf("The test could not install the existing transcript: %v", err)
	}
	fixture.startDaemon(settings)
	checkpoints := map[string]string{}
	fixture.waitFor("checkpoints for all uncompressed rollouts and the existing chat", func() bool {
		for _, threadID := range threadIDs {
			content, present := fixture.checkpointFor(newestCodexID(threadID))
			if !present {
				return false
			}
			checkpoints[threadID] = content
		}
		return fixture.checkpointPresent(newestExistingSession)
	})
	rowsBefore := map[string]int64{}
	for _, threadID := range threadIDs {
		count, err := fixture.rowsFor(newestCodexID(threadID))
		if err != nil {
			t.Fatalf("The test could not count Milvus rows for a rollout: %v", err)
		}
		rowsBefore[threadID] = count
	}
	sentBefore := []string{}
	for _, record := range fixture.passRecords() {
		sentBefore = append(sentBefore, record.SentConversationIDs...)
	}

	// The folder rename makes the compressed rollouts and new chat available to the
	// next scan together.
	replacement := filepath.Join(fixture.home, "sessions-replacement")
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("The test could not create the zstd encoder: %v", err)
	}
	defer func() { _ = encoder.Close() }()
	for _, threadID := range threadIDs {
		path := newestRolloutPath(replacement, threadID) + ".zst"
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("The test could not create the replacement folder: %v", err)
		}
		if err := writeNewestFile(path, encoder.EncodeAll([]byte(bodies[threadID]), nil), oldModified); err != nil {
			t.Fatalf("The test could not write the compressed rollout file: %v", err)
		}
	}
	newBody, err := newestRolloutBody(newestRolloutNewID, 1, 0)
	if err != nil {
		t.Fatalf("The test could not build the new rollout content: %v", err)
	}
	if err := writeNewestFile(newestRolloutPath(replacement, newestRolloutNewID), []byte(newBody), time.Now().Add(-newestFreshAge)); err != nil {
		t.Fatalf("The test could not write the new rollout file: %v", err)
	}
	if err := os.Rename(sessions, filepath.Join(fixture.home, "sessions-previous")); err != nil {
		t.Fatalf("The test could not rename the uncompressed sessions folder: %v", err)
	}
	if err := os.Rename(replacement, sessions); err != nil {
		t.Fatalf("The test could not rename the replacement sessions folder into place: %v", err)
	}
	arrivals := startNewestArrivals(fixture)

	newID := newestCodexID(newestRolloutNewID)
	fixture.waitFor("a checkpoint for the new chat", func() bool {
		_, present := fixture.checkpointFor(newID)
		return present
	})
	// A recheck finishes when the worker saves a new checkpoint or logs its ID in an
	// unchanged_pinned record. A pinned recheck retains its previous checkpoint.
	rechecksFinished := func() int {
		pinned := map[string]bool{}
		for _, record := range fixture.logRecords(newestPinnedEvent) {
			for _, conversationID := range record.PinnedIDs {
				pinned[conversationID] = true
			}
		}
		finished := 0
		for _, threadID := range threadIDs {
			conversationID := newestCodexID(threadID)
			content, present := fixture.checkpointFor(conversationID)
			if pinned[conversationID] || (present && content != checkpoints[threadID]) {
				finished++
			}
		}
		return finished
	}

	// The waiting chat has an earlier file time than the existing chat from the
	// first manifest.
	waitingModified := time.Now().Add(-newestWaitingAge)
	if err := installNewestTranscript(fixture.home, newestWaitingSession, newestTranscript(newestWaitingSession, newestWaitingMessages, 0, waitingModified), waitingModified); err != nil {
		t.Fatalf("The test could not install the waiting transcript: %v", err)
	}
	fixture.waitForFirstListing(newestConversationID(newestWaitingSession), 1)
	rechecksAtWaiting := -1
	fixture.waitFor("a checkpoint for the waiting chat", func() bool {
		finished := rechecksFinished()
		if fixture.checkpointPresent(newestWaitingSession) {
			rechecksAtWaiting = finished
			return true
		}
		return false
	})
	if rechecksAtWaiting == newestRechecks {
		t.Fatalf("The waiting chat finished after all %d compressed rechecks; the test expected it to finish before the rechecks completed.", newestRechecks)
	}

	// The test installs the second chat after the worker lists the rechecks.
	secondBody, err := newestRolloutBody(newestRolloutSecondID, 1, 0)
	if err != nil {
		t.Fatalf("The test could not build the second rollout content: %v", err)
	}
	if err := writeNewestFile(newestRolloutPath(sessions, newestRolloutSecondID), []byte(secondBody), time.Now().Add(-newestFreshAge)); err != nil {
		t.Fatalf("The test could not write the second rollout file: %v", err)
	}
	secondID := newestCodexID(newestRolloutSecondID)
	rechecksAtSecond := -1
	fixture.waitFor("a checkpoint for the second new chat", func() bool {
		finished := rechecksFinished()
		_, present := fixture.checkpointFor(secondID)
		if present {
			rechecksAtSecond = finished
		}
		return present
	})
	if rechecksAtSecond == newestRechecks {
		t.Fatalf("The second new chat finished after all %d compressed rechecks; the test expected it to finish before the rechecks completed.", newestRechecks)
	}
	t.Logf("The worker had finished %d compressed rechecks when the waiting chat finished and %d when the second chat finished, out of %d rechecks.", rechecksAtWaiting, rechecksAtSecond, newestRechecks)
	fixture.waitFor("completion of every compressed rollout recheck", func() bool { return rechecksFinished() == newestRechecks })
	arrivals.halt()

	records := fixture.passRecords()
	sentOrder := []string{}
	for _, record := range records {
		sentOrder = append(sentOrder, record.SentConversationIDs...)
	}
	sentOrder = sentOrder[len(sentBefore):]
	newPosition := slices.Index(sentOrder, newID)
	rechecksBefore := 0
	for _, identifier := range sentOrder[:max(newPosition, 0)] {
		if slices.ContainsFunc(threadIDs, func(threadID string) bool { return identifier == newestCodexID(threadID) }) {
			rechecksBefore++
		}
	}
	if newPosition < 0 || rechecksBefore > newestRecheckLead {
		t.Fatalf("The new chat has delivery position %d after %d compressed rechecks; the test expected a delivery after at most %d rechecks.", newPosition, rechecksBefore, newestRecheckLead)
	}
	for _, threadID := range threadIDs {
		count, countErr := fixture.rowsFor(newestCodexID(threadID))
		if countErr != nil {
			t.Fatalf("The test could not count Milvus rows for a rollout: %v", countErr)
		}
		if count != rowsBefore[threadID] {
			t.Fatalf("Rollout %s has %d Milvus rows after compression; the test expected %d.", threadID, count, rowsBefore[threadID])
		}
	}
	fixture.requireExactMatch(newID, newestToken(newestRolloutNewID, 0), 0)
	fixture.requireExactMatch(secondID, newestToken(newestRolloutSecondID, 0), 0)
	fixture.requireExactSearch(newestWaitingSession, newestWaitingMessages-1)
}

// checkChangedChatJoinsQueue changes a chat after an empty needed listing and
// checks that the worker queues it in a new cohort.
func checkChangedChatJoinsQueue(t *testing.T, fixture *newestFixture, gate *newestEmbeddingGate) {
	t.Helper()
	changedID := newestConversationID(newestChangedSession)
	fixture.waitForNoNeededChats()
	listedBefore := fixture.firstListedCount(changedID)
	if listedBefore == 0 {
		t.Fatalf("The daemon did not first list chat %s before the content change.", newestChangedSession)
	}
	// The closed gate blocks the first older chat's embedding request. The next
	// listing includes the changed chat while other older chats still need delivery.
	gate.close()
	older := installNewestOlder(t, fixture.home, newestShortBacklog)
	fixture.waitFor("an embedding request waiting for the test gate to open", func() bool { return gate.blocked.Load() > 0 })

	changedModified := time.Now().Add(-newestFreshAge)
	body := newestTranscript(newestChangedSession, newestChangedMessages, 0, changedModified)
	path := filepath.Join(newestProjectDirectory(fixture.home), newestChangedSession+".jsonl")
	if err := writeNewestFile(path, []byte(body), changedModified); err != nil {
		t.Fatalf("The test could not update the chat transcript: %v", err)
	}
	time.Sleep(newestIndexRefreshWait)
	gate.open()
	fixture.waitForFirstListing(changedID, listedBefore+1)

	later := make([]string, 0, newestLaterChats)
	laterModified := time.Now().Add(-newestLaterAge)
	for position := range newestLaterChats {
		session := fmt.Sprintf("%s%04d", newestFreshPrefix, position)
		later = append(later, session)
		if err := installNewestTranscript(fixture.home, session, newestTranscript(session, 1, newestBallastBytes, laterModified), laterModified); err != nil {
			t.Fatalf("The test could not install a later transcript: %v", err)
		}
	}

	fixture.waitForRows(newestChangedSession, newestChangedMessages)
	fixture.waitFor("the backlog", func() bool { return fixture.finished(older) == newestShortBacklog })
	fixture.waitFor("checkpoints for all later chats", func() bool { return fixture.finished(later) == newestLaterChats })
	fixture.requireOlderRows(older)
	fixture.requireExactSearch(newestChangedSession, newestChangedMessages-1)

	records := fixture.passRecords()
	changedSends := newestPassesSending(records, changedID)
	lastOlder := newestPassesSending(records, newestConversationID(older[len(older)-1]))
	if len(changedSends) == 0 || len(lastOlder) == 0 || changedSends[len(changedSends)-1] > lastOlder[len(lastOlder)-1] {
		t.Fatalf("The changed chat has delivery passes %v, and the last older chat has delivery passes %v; the test expected the changed chat to finish first.", changedSends, lastOlder)
	}
}

// TestMilvusFeederQueuesChangedChatAfterPinnedEmptyListing checks that a pinned
// chat joins a new cohort after its content changes.
func TestMilvusFeederQueuesChangedChatAfterPinnedEmptyListing(t *testing.T) {
	gate := startNewestEmbeddingGate(t)
	fixture, settings := newestMilvusSettingsFor(t, sandboxFastIntervals, gate.baseURL)
	firstModified := time.Now().Add(-newestFirstAge)
	if err := installNewestTranscript(fixture.home, newestChangedSession, newestTranscript(newestChangedSession, newestWaitingMessages, 0, firstModified), firstModified); err != nil {
		t.Fatalf("The test could not install the transcript: %v", err)
	}
	fixture.startDaemon(settings)
	fixture.waitFor("the first delivery", func() bool { return fixture.checkpointPresent(newestChangedSession) })
	fixture.waitForNoNeededChats()

	retimed := time.Now().Add(-newestRetimedAge)
	path := filepath.Join(newestProjectDirectory(fixture.home), newestChangedSession+".jsonl")
	if err := os.Chtimes(path, retimed, retimed); err != nil {
		t.Fatalf("The test could not change the transcript modification time: %v", err)
	}
	fixture.waitFor("a sync pass with an unchanged_pinned count above zero", func() bool {
		return slices.ContainsFunc(fixture.passRecords(), func(record newestPassRecord) bool { return record.UnchangedPinned > 0 })
	})
	checkChangedChatJoinsQueue(t, fixture, gate)
}

// TestMilvusFeederQueuesChangedChatAfterZeroDocumentListing checks that a chat
// joins a new cohort after it gains content that the worker can deliver.
func TestMilvusFeederQueuesChangedChatAfterZeroDocumentListing(t *testing.T) {
	gate := startNewestEmbeddingGate(t)
	fixture, settings := newestMilvusSettingsFor(t, sandboxFastIntervals, gate.baseURL)
	firstModified := time.Now().Add(-newestFirstAge)
	body := newestMessageLine(newestChangedSession, 0, newestMetaField, firstModified)
	if err := installNewestTranscript(fixture.home, newestChangedSession, body, firstModified); err != nil {
		t.Fatalf("The test could not install the transcript: %v", err)
	}
	fixture.startDaemon(settings)
	fixture.waitFor("a sync pass with a policy_skipped count above zero", func() bool {
		return slices.ContainsFunc(fixture.passRecords(), func(record newestPassRecord) bool { return record.PolicySkipped > 0 })
	})
	if count, err := fixture.rows(newestChangedSession); err == nil && count != 0 {
		t.Fatalf("The control record has %d Milvus rows; the test expected zero.", count)
	}
	checkChangedChatJoinsQueue(t, fixture, gate)
}

// TestMilvusFeederKeepsQueuedChatWhenCheckpointWriteFails checks that the worker
// retries a queued chat after the checkpoint write fails.
func TestMilvusFeederKeepsQueuedChatWhenCheckpointWriteFails(t *testing.T) {
	fixture, settings := newestMilvusSettings(t, sandboxFastIntervals)
	older := installNewestOlder(t, fixture.home, newestShortBacklog)
	fixture.startDaemon(settings)
	fixture.waitFor("the first older chat", func() bool { return fixture.finished(older) > 0 })

	directory := fixture.checkpointDirectory()
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatalf("The test could not make the checkpoint folder read-only: %v", err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			if err := os.Chmod(directory, 0o700); err != nil {
				t.Errorf("The test could not make the checkpoint folder writable: %v", err)
			}
		}
	}
	defer restore()

	waitingID := newestConversationID(newestWaitingSession)
	waitingModified := time.Now().Add(-newestLongWaitAge)
	if err := installNewestTranscript(fixture.home, newestWaitingSession, newestTranscript(newestWaitingSession, newestWaitingMessages, 0, waitingModified), waitingModified); err != nil {
		t.Fatalf("The test could not install the waiting transcript: %v", err)
	}
	fixture.waitForRows(newestWaitingSession, newestWaitingMessages)
	requireNoAcceptedDelivery := func() {
		t.Helper()
		if sent := fixture.sentCount(waitingID); sent != 0 {
			t.Fatalf("The upsert reported success %d times before the worker saved a checkpoint for the waiting chat.", sent)
		}
	}
	fixture.waitFor("a sync pass with a failed count above zero", func() bool {
		requireNoAcceptedDelivery()
		return slices.ContainsFunc(fixture.passRecords(), func(record newestPassRecord) bool { return record.Failed > 0 })
	})
	if fixture.checkpointPresent(newestWaitingSession) {
		t.Fatalf("The worker saved the waiting chat's checkpoint in a read-only folder.")
	}

	later := make([]string, 0, newestShortBacklog)
	laterModified := time.Now().Add(-newestFreshAge)
	for position := range newestShortBacklog {
		session := fmt.Sprintf("%s%04d", newestFreshPrefix, position)
		later = append(later, session)
		if err := installNewestTranscript(fixture.home, session, newestTranscript(session, 1, newestBallastBytes, laterModified), laterModified); err != nil {
			t.Fatalf("The test could not install a later transcript: %v", err)
		}
	}
	fixture.waitFor("Milvus rows for at least one later chat", func() bool {
		requireNoAcceptedDelivery()
		return slices.ContainsFunc(later, func(session string) bool {
			count, err := fixture.rows(session)
			return err == nil && count > 0
		})
	})

	// The test changes the source file time without changing its messages. The
	// worker must retry the earlier failed upsert.
	retimed := time.Now().Add(-newestRetimedWaitAge)
	path := filepath.Join(newestProjectDirectory(fixture.home), newestWaitingSession+".jsonl")
	if err := os.Chtimes(path, retimed, retimed); err != nil {
		t.Fatalf("The test could not change the waiting transcript's modification time: %v", err)
	}
	restore()

	laterAtWaiting := -1
	fixture.waitFor("a checkpoint for the waiting chat", func() bool {
		finished := fixture.finished(later)
		if fixture.checkpointPresent(newestWaitingSession) {
			laterAtWaiting = finished
			return true
		}
		return false
	})
	if laterAtWaiting == newestShortBacklog {
		t.Fatalf("The worker saved the waiting chat's checkpoint after all %d later chats finished; the test expected an earlier checkpoint.", newestShortBacklog)
	}
	if listed := fixture.firstListedCount(waitingID); listed != 1 {
		t.Fatalf("The daemon first listed the waiting chat %d times; the test expected one listing.", listed)
	}
	count, err := fixture.rows(newestWaitingSession)
	if err != nil {
		t.Fatalf("The test could not count Milvus rows for a waiting chat: %v", err)
	}
	if count != newestWaitingMessages {
		t.Fatalf("The waiting chat has %d Milvus rows; the test expected %d.", count, newestWaitingMessages)
	}
	// A failed upsert does not log completion. A retry completion record lists the
	// chat only if the retry wrote its rows again.
	for _, record := range fixture.logRecords(newestUpsertEvent) {
		for _, written := range record.WrittenChats {
			if strings.HasPrefix(written, waitingID+"=") {
				t.Fatalf("The retry wrote rows for the waiting chat again: %s", written)
			}
		}
	}
	fixture.requireExactSearch(newestWaitingSession, newestWaitingMessages-1)
	fixture.waitFor("checkpoints for all older chats", func() bool { return fixture.finished(older) == newestShortBacklog })
	fixture.requireOlderRows(older)
}
