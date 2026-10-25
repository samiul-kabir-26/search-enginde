package searchapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/stages/stagestest"
)

type memSink struct {
	mu   sync.Mutex
	rows []ShadowRow
}

func (m *memSink) Write(r ShadowRow) {
	m.mu.Lock()
	m.rows = append(m.rows, r)
	m.mu.Unlock()
}

func (m *memSink) n() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

func TestShadowRunsOnSampledRequests(t *testing.T) {
	h := newHarness(t)
	shadow := stagestest.Resolved()
	shadow.ConfigVersion = 4
	shadow.Config.Rank.Keep = 10
	h.res.Shadow, h.res.ShadowRate = shadow, 1
	sink := &memSink{}
	p, _, _ := stagestest.Providers()
	h.svc.Shadow = NewShadow(h.engine, fixedProviders{p}, sink, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))

	tok := h.token(t, auth.ScopeSearchRead)
	code, sr, _ := h.search(t, tok, `{"q":"red leather bag"}`)
	if code != http.StatusOK {
		t.Fatal(code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for sink.n() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sink.n() != 1 {
		t.Fatalf("shadow rows = %d", sink.n())
	}
	row := sink.rows[0]
	if row.ShadowVersion != 4 || row.ActiveVersion != 3 || row.RequestID != sr.RequestID || len(row.ActiveTop) == 0 || row.OverlapAt10 <= 0 {
		t.Errorf("row = %+v (request %s)", row, sr.RequestID)
	}
	_, _, _ = h.search(t, tok, `{"q":"red leather bag","page":{"offset":0}}`)
	time.Sleep(50 * time.Millisecond)
	if sink.n() != 1 {
		t.Error("snapshot hits must not run the shadow again")
	}

	h.res.ShadowRate = 0
	_, _, _ = h.search(t, tok, `{"q":"another query"}`)
	time.Sleep(50 * time.Millisecond)
	if sink.n() != 1 {
		t.Error("a zero sample rate must not run the shadow")
	}
	var nilShadow *Shadow
	nilShadow.Maybe(h.res, pipeline.Query{}, nil, 0, "x")
}

func TestPlaygroundPinsVersion(t *testing.T) {
	h := newHarness(t)
	do := func(scopes []string, ref string) (int, string) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, h.srv.URL+"/v1/search", strings.NewReader(`{"q":"red bag","page":{"size":2}}`))
		req.Header.Set("Authorization", "Bearer "+h.token(t, scopes...))
		req.Header.Set(HeaderConfigVersion, ref)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := do([]string{auth.ScopeSearchRead, auth.ScopeSearchDebug}, "7"); code != http.StatusForbidden {
		t.Errorf("the version header needs search:playground: %d", code)
	}
	code, body := do([]string{auth.ScopeSearchRead, auth.ScopeSearchPlayground}, "7")
	if code != http.StatusOK || !strings.Contains(body, `"config_version":7`) || !strings.Contains(body, `"trace":{`) || !strings.Contains(body, `"next_cursor":null`) {
		t.Errorf("playground v7: %d %s", code, body[:min(300, len(body))])
	}
	code, body = do([]string{auth.ScopeSearchRead, auth.ScopeSearchPlayground}, "draft")
	if code != http.StatusOK || !strings.Contains(body, `"config_version":0`) {
		t.Errorf("playground draft: %d %s", code, body[:min(200, len(body))])
	}
	if code, _ := do([]string{auth.ScopeSearchRead, auth.ScopeSearchPlayground}, "99"); code != http.StatusNotFound {
		t.Errorf("unknown version: %d", code)
	}
}
