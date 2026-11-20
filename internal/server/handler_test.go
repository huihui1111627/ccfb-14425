package server_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aquaprofile/internal/server"
	"aquaprofile/internal/store"
	"aquaprofile/internal/symbolize"
)

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st := store.New()
	srv := httptest.NewServer(server.New(st))
	t.Cleanup(srv.Close)
	return srv, st
}

func post(t *testing.T, srv *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestModuleAndSampleUploadEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)

	// Invalid JSON -> 400.
	resp, err := http.Post(srv.URL+"/api/modules", "application/json", strings.NewReader("{bad"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad json status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Missing program -> 400.
	resp = post(t, srv, "/api/modules", store.ModuleUpload{Version: "v1"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing program status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Valid module upload.
	resp = post(t, srv, "/api/modules", store.ModuleUpload{
		Program: "p", Version: "v1", Pid: 1,
		Modules: []symbolize.Module{{
			Name: "m.bin", Base: 0x1000, Size: 0x1000,
			Symbols: []symbolize.Symbol{{Name: "f", Addr: 0x1000, Size: 0x100}},
		}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("module upload status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Sample batch with bad period -> 400.
	bad := store.Batch{Program: "p", Version: "v1", Pid: 1}
	resp = post(t, srv, "/api/samples", bad)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("zero period status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Valid batch -> accepted count echoed.
	good := store.Batch{
		Program: "p", Version: "v1", Pid: 1, PeriodNs: 1000,
		Samples: []store.Sample{
			{TimeNs: 1, Addresses: []uint64{0x1010}, Labels: map[string]string{"tank": "T01"}},
			{TimeNs: 2, Addresses: []uint64{0x1020}, Labels: map[string]string{"tank": "T02"}},
		},
	}
	resp = post(t, srv, "/api/samples", good)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sample upload status = %d", resp.StatusCode)
	}
	var ack map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ack["accepted"].(float64) != 2 {
		t.Fatalf("accepted = %v, want 2", ack["accepted"])
	}
}

func TestProgramsAndHotEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)
	post(t, srv, "/api/modules", store.ModuleUpload{
		Program: "p", Version: "v1", Pid: 1,
		Modules: []symbolize.Module{{
			Name: "m.bin", Base: 0x1000, Size: 0x1000,
			Symbols: []symbolize.Symbol{
				{Name: "hot", Addr: 0x1000, Size: 0x100},
				{Name: "cold", Addr: 0x1200, Size: 0x100},
			},
		}},
	})
	batch := store.Batch{Program: "p", Version: "v1", Pid: 1, PeriodNs: 10}
	for i := 0; i < 90; i++ {
		batch.Samples = append(batch.Samples, store.Sample{
			TimeNs: uint64(i + 1), Addresses: []uint64{0x1010},
			Labels: map[string]string{"tank": "T01"},
		})
	}
	for i := 0; i < 10; i++ {
		batch.Samples = append(batch.Samples, store.Sample{
			TimeNs: uint64(100 + i), Addresses: []uint64{0x1210},
			Labels: map[string]string{"tank": "T01"},
		})
	}
	post(t, srv, "/api/samples", batch).Body.Close()

	// Catalog.
	resp, err := http.Get(srv.URL + "/api/programs")
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Programs []struct {
			Name     string   `json:"name"`
			Versions []string `json:"versions"`
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(catalog.Programs) != 1 || catalog.Programs[0].Name != "p" ||
		len(catalog.Programs[0].Versions) != 1 || catalog.Programs[0].Versions[0] != "v1" {
		t.Fatalf("catalog wrong: %+v", catalog)
	}

	// Hot ranking: hot() must lead with a 90% self share.
	resp, err = http.Get(srv.URL + "/api/hot?program=p&version=v1")
	if err != nil {
		t.Fatal(err)
	}
	var hot struct {
		Rows []struct {
			Func      string  `json:"func"`
			SelfShare float64 `json:"self_share"`
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&hot); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(hot.Rows) != 2 || hot.Rows[0].Func != "hot" {
		t.Fatalf("hot ranking wrong: %+v", hot.Rows)
	}
	if !floatEq(hot.Rows[0].SelfShare, 0.9) {
		t.Fatalf("hot self share = %f, want 0.9", hot.Rows[0].SelfShare)
	}

	// Missing query params -> 400 on every read endpoint.
	for _, path := range []string{
		"/api/flame?program=p",
		"/api/hot?version=v1",
		"/api/diff?program=p&old_version=v1",
	} {
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", path, r.StatusCode)
		}
		r.Body.Close()
	}
}

func TestLabelFilterThroughHTTP(t *testing.T) {
	srv, _ := newTestServer(t)
	post(t, srv, "/api/modules", store.ModuleUpload{
		Program: "p", Version: "v1", Pid: 1,
		Modules: []symbolize.Module{{
			Name: "m.bin", Base: 0x1000, Size: 0x1000,
			Symbols: []symbolize.Symbol{{Name: "f", Addr: 0x1000, Size: 0x100}},
		}},
	})
	batch := store.Batch{Program: "p", Version: "v1", Pid: 1, PeriodNs: 1}
	for i := 0; i < 5; i++ {
		batch.Samples = append(batch.Samples, store.Sample{
			TimeNs: uint64(i + 1), Addresses: []uint64{0x1010},
			Labels: map[string]string{"tank": "T01"},
		})
	}
	for i := 0; i < 3; i++ {
		batch.Samples = append(batch.Samples, store.Sample{
			TimeNs: uint64(10 + i), Addresses: []uint64{0x1010},
			Labels: map[string]string{"tank": "T02"},
		})
	}
	post(t, srv, "/api/samples", batch).Body.Close()

	resp, err := http.Get(srv.URL + "/api/flame?program=p&version=v1&label.tank=T02")
	if err != nil {
		t.Fatal(err)
	}
	var flame struct {
		SampleCount int64 `json:"sample_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&flame); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if flame.SampleCount != 3 {
		t.Fatalf("label filter sample count = %d, want 3", flame.SampleCount)
	}
}

func TestStaticUIServed(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("UI status = %d", resp.StatusCode)
	}
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "<html") && !strings.Contains(string(buf[:n]), "<!DOCTYPE") {
		t.Fatalf("UI root did not serve HTML: %q", string(buf[:n]))
	}
}
