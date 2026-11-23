package codex

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	sq "github.com/Masterminds/squirrel"
	"github.com/gorilla/websocket"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

func TestWebsocketTransportRecordsExchangeAfterEmitterFailure(t *testing.T) {
	store, databasePath := openCodexCaptureStore(t)
	t.Cleanup(func() {
		if err := store.Close(context.Background(), "test"); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewUnstartedServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			upgrader := websocket.Upgrader{}
			connection, err := upgrader.Upgrade(writer, request, nil)
			if err != nil {
				t.Error(err)

				return
			}

			defer func() { _ = connection.Close() }()
			if _, _, err := connection.ReadMessage(); err != nil {
				t.Error(err)

				return
			}

			frames := []string{
				`{"type":"response.output_text.delta","delta":"first"}`,
				`{"type":"response.output_text.delta","delta":"` + strings.Repeat(
					"x",
					256*1024,
				) + `"}`,
			}
			for _, frame := range frames {
				if err := connection.WriteMessage(
					websocket.TextMessage,
					[]byte(frame),
				); err != nil {
					return
				}
			}
		}),
	)
	if err := server.Listener.Close(); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}

	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	emitterError := errors.New("client stopped reading")
	pprof.Do(
		t.Context(),
		pprof.Labels("cleanup_test", t.Name()),
		func(ctx context.Context) {
			_, err = RunWebsocketTransportEvents(
				ctx,
				WebsocketTransportConfig{
					URL:          "ws" + strings.TrimPrefix(server.URL, "http"),
					CaptureStore: store,
				},
				ResponseCreateWsRequest{Type: "response.create", Model: "test-model"},
				func(adapterrender.Event) error {
					deadline := time.Now().Add(5 * time.Second)
					for time.Now().Before(deadline) {
						var stacks bytes.Buffer
						if err := pprof.Lookup("goroutine").WriteTo(&stacks, 1); err != nil {
							t.Fatal(err)
						}

						for _, stack := range strings.Split(stacks.String(), "\n\n") {
							if strings.Contains(stack, t.Name()) &&
								strings.Contains(stack, "io.(*pipe).write") {
								return emitterError
							}
						}

						time.Sleep(time.Millisecond)
					}

					t.Fatal("The websocket reader did not start writing the next frame.")

					return emitterError
				},
			)
		},
	)
	if !errors.Is(err, emitterError) {
		t.Fatalf("transport error = %v, want %v", err, emitterError)
	}

	database, err := sql.Open("sqlite3", "file:"+databasePath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = database.Close() })
	query, arguments, err := sq.Select("status").From("requests").
		Where(sq.Eq{
			"client": captureClientCodex,
		}).ToSql()
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		var status int
		err := database.QueryRowContext(t.Context(), query, arguments...).Scan(&status)
		if err == nil {
			if status != 0 {
				t.Fatalf("capture status = %d, want 0 for the interrupted exchange", status)
			}

			return
		}

		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}

		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("The websocket reader did not finish recording after the emitter failed.")
		}
	}
}
