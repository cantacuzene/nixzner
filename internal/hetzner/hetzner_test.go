package hetzner

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

type fakeAPI struct {
	mu              sync.Mutex
	serverTypes     map[string]string          // name -> architecture
	stock           map[string]map[string]bool // server type -> location -> available
	prices          map[string]string          // server type -> gross hourly price in every location
	deprecated      map[string]string          // server type -> location where it is deprecated
	locations       map[string]bool
	images          map[string]bool // "name/arch"
	sshKeys         map[string]bool
	firewalls       map[string]bool
	networks        map[string]bool
	existingServers map[string]bool
	actionStatus    string
	createBody      map[string]any
	updateBody      map[string]any
	deleted         []string
	imageQueries    []string
}

func set(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func newFakeAPI(t *testing.T) (*Client, *fakeAPI) {
	t.Helper()
	f := &fakeAPI{
		serverTypes:     map[string]string{"cx32": "x86", "cax11": "arm"},
		stock:           map[string]map[string]bool{"cx32": {"fsn1": true}, "cax11": {"fsn1": true}},
		prices:          map[string]string{"cx32": "0.0136", "cax11": "0.0096"},
		deprecated:      map[string]string{},
		locations:       set("fsn1"),
		images:          set("debian-12/x86", "debian-12/arm"),
		sshKeys:         set("admin"),
		firewalls:       set("web"),
		networks:        set("internal"),
		existingServers: map[string]bool{},
		actionStatus:    "success",
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c := New("test-token",
		hcloud.WithEndpoint(srv.URL),
		hcloud.WithPollOpts(hcloud.PollOpts{BackoffFunc: hcloud.ConstantBackoff(time.Millisecond)}),
	)
	return c, f
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func named(ok bool, id int, name string) []any {
	if !ok {
		return nil
	}
	return []any{map[string]any{"id": id, "name": name}}
}

func serverJSON(id int, name string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "status": "initializing",
		"public_net": map[string]any{
			"ipv4": map[string]any{"id": 1, "ip": "203.0.113.10"},
			"ipv6": map[string]any{"id": 2, "ip": "2001:db8:1:2::/64"},
		},
		"labels": map[string]any{},
	}
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	name := q.Get("name")
	get := r.Method == http.MethodGet
	switch {
	case get && r.URL.Path == "/server_types":
		var items []any
		for _, st := range slices.Sorted(maps.Keys(f.serverTypes)) {
			if name != "" && st != name {
				continue
			}
			var locs, prices []any
			for loc, available := range f.stock[st] {
				l := map[string]any{"id": 2, "name": loc, "available": available}
				if f.deprecated[st] == loc {
					l["deprecation"] = map[string]any{"announced": "2026-01-01T00:00:00Z", "unavailable_after": "2026-12-01T00:00:00Z"}
				}
				locs = append(locs, l)
				prices = append(prices, map[string]any{"location": loc, "price_hourly": map[string]any{"net": "0", "gross": f.prices[st]}})
			}
			items = append(items, map[string]any{"id": 1, "name": st, "architecture": f.serverTypes[st],
				"cores": 2, "memory": 4.0, "locations": locs, "prices": prices})
		}
		writeJSON(w, 200, map[string]any{"server_types": items})
	case get && r.URL.Path == "/locations":
		var items []any
		for _, loc := range slices.Sorted(maps.Keys(f.locations)) {
			if name == "" || loc == name {
				items = append(items, map[string]any{"id": 2, "name": loc, "city": "City of " + loc, "country": "DE"})
			}
		}
		writeJSON(w, 200, map[string]any{"locations": items})
	case get && r.URL.Path == "/images":
		key := name + "/" + q.Get("architecture")
		f.imageQueries = append(f.imageQueries, key)
		var items []any
		if f.images[key] {
			items = []any{map[string]any{"id": 10, "name": name, "type": "system", "architecture": q.Get("architecture")}}
		}
		writeJSON(w, 200, map[string]any{"images": items})
	case get && r.URL.Path == "/ssh_keys":
		var items []any
		for _, key := range slices.Sorted(maps.Keys(f.sshKeys)) {
			if name == "" || key == name {
				items = append(items, map[string]any{"id": 5, "name": key})
			}
		}
		writeJSON(w, 200, map[string]any{"ssh_keys": items})
	case get && r.URL.Path == "/firewalls":
		writeJSON(w, 200, map[string]any{"firewalls": named(f.firewalls[name], 7, name)})
	case get && r.URL.Path == "/networks":
		writeJSON(w, 200, map[string]any{"networks": named(f.networks[name], 8, name)})
	case get && r.URL.Path == "/servers":
		var items []any
		if f.existingServers[name] {
			items = []any{serverJSON(99, name)}
		}
		writeJSON(w, 200, map[string]any{"servers": items})
	case r.Method == http.MethodPost && r.URL.Path == "/servers":
		_ = json.NewDecoder(r.Body).Decode(&f.createBody)
		writeJSON(w, 201, map[string]any{
			"server":       serverJSON(42, f.createBody["name"].(string)),
			"action":       map[string]any{"id": 100, "status": "running", "command": "create_server"},
			"next_actions": []any{map[string]any{"id": 101, "status": "running", "command": "start_server"}},
		})
	case get && r.URL.Path == "/actions":
		var actions []any
		for _, id := range q["id"] {
			n, _ := strconv.Atoi(id)
			a := map[string]any{"id": n, "status": f.actionStatus, "command": "x"}
			if f.actionStatus == "error" {
				a["error"] = map[string]any{"code": "action_failed", "message": "boom"}
			}
			actions = append(actions, a)
		}
		writeJSON(w, 200, map[string]any{"actions": actions})
	case r.Method == http.MethodDelete && r.URL.Path == "/servers/42":
		f.deleted = append(f.deleted, "42")
		writeJSON(w, 200, map[string]any{"action": map[string]any{"id": 102, "status": "running", "command": "delete_server"}})
	case r.Method == http.MethodPut && r.URL.Path == "/servers/42":
		_ = json.NewDecoder(r.Body).Decode(&f.updateBody)
		writeJSON(w, 200, map[string]any{"server": serverJSON(42, "web1")})
	default:
		writeJSON(w, 404, map[string]any{"error": map[string]any{"code": "not_found", "message": r.Method + " " + r.URL.String()}})
	}
}

func validRequest() CreateRequest {
	return CreateRequest{
		Name: "web1", ServerType: "cx32", Location: "fsn1", Image: "debian-12", SSHKey: "admin",
		Firewalls: []string{"web"}, Networks: []string{"internal"},
		Labels: map[string]string{"managed-by": "nixzner"},
	}
}

func TestPreflightOK(t *testing.T) {
	c, _ := newFakeAPI(t)
	if err := c.Preflight(context.Background(), validRequest()); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightReportsEveryProblem(t *testing.T) {
	c, f := newFakeAPI(t)
	f.existingServers["web1"] = true
	r := validRequest()
	r.ServerType = "nope"
	r.Firewalls = []string{"web", "missing-fw"}
	err := c.Preflight(context.Background(), r)
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{`server type "nope" not found`, `firewall "missing-fw" not found`, `server "web1" already exists`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestPreflightLooksUpImageForServerTypeArchitecture(t *testing.T) {
	c, f := newFakeAPI(t)
	r := validRequest()
	r.ServerType = "cax11"
	if err := c.Preflight(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.imageQueries, "debian-12/arm") {
		t.Fatalf("image queries = %v, want debian-12/arm", f.imageQueries)
	}
}

func TestPreflightRejectsOutOfStockServerType(t *testing.T) {
	c, f := newFakeAPI(t)
	f.stock["cx32"] = map[string]bool{"fsn1": false, "nbg1": true, "hel1": true, "ash": false}
	err := c.Preflight(context.Background(), validRequest())
	want := `server type "cx32" is out of stock in fsn1 (in stock in: hel1, nbg1)`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to mention %q", err, want)
	}
}

func TestPreflightRejectsServerTypeOutOfStockEverywhere(t *testing.T) {
	c, f := newFakeAPI(t)
	f.stock["cx32"] = map[string]bool{"fsn1": false, "nbg1": false}
	err := c.Preflight(context.Background(), validRequest())
	want := `server type "cx32" is out of stock in fsn1 (not in stock in any location)`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to mention %q", err, want)
	}
}

func TestPreflightRejectsServerTypeNotOfferedInLocation(t *testing.T) {
	c, f := newFakeAPI(t)
	f.stock["cx32"] = map[string]bool{"nbg1": true}
	err := c.Preflight(context.Background(), validRequest())
	want := `server type "cx32" is not offered in fsn1 (in stock in: nbg1)`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to mention %q", err, want)
	}
}

func TestPreflightSkipsStockCheckWithoutLocationData(t *testing.T) {
	c, f := newFakeAPI(t)
	delete(f.stock, "cx32")
	if err := c.Preflight(context.Background(), validRequest()); err != nil {
		t.Fatal(err)
	}
}

func TestCreateServer(t *testing.T) {
	c, f := newFakeAPI(t)
	srv, err := c.CreateServer(context.Background(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := Server{ID: 42, Name: "web1", IPv4: "203.0.113.10", IPv6: "2001:db8:1:2::1"}
	if srv != want {
		t.Fatalf("server = %+v, want %+v", srv, want)
	}
	b := f.createBody
	if b["name"] != "web1" || b["start_after_create"] != true {
		t.Errorf("create body = %v", b)
	}
	labels := map[string]string{}
	for k, v := range b["labels"].(map[string]any) {
		labels[k] = v.(string)
	}
	if !maps.Equal(labels, map[string]string{"managed-by": "nixzner"}) {
		t.Errorf("labels = %v", labels)
	}
	for _, field := range []string{"ssh_keys", "firewalls", "networks"} {
		if items, _ := b[field].([]any); len(items) != 1 {
			t.Errorf("%s = %v, want one entry", field, b[field])
		}
	}
}

func TestCreateServerReturnsServerWhenStartFails(t *testing.T) {
	c, f := newFakeAPI(t)
	f.actionStatus = "error"
	srv, err := c.CreateServer(context.Background(), validRequest())
	if err == nil {
		t.Fatal("want error")
	}
	if srv.ID != 42 {
		t.Fatalf("server ID = %d, want 42 so the caller can delete it", srv.ID)
	}
}

func TestDeleteServer(t *testing.T) {
	c, f := newFakeAPI(t)
	if err := c.DeleteServer(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.deleted, []string{"42"}) {
		t.Fatalf("deleted = %v", f.deleted)
	}
}

func TestDeleteServerNotFoundIsNotAnError(t *testing.T) {
	c, _ := newFakeAPI(t)
	if err := c.DeleteServer(context.Background(), 404); err != nil {
		t.Fatal(err)
	}
}

func TestSetLabels(t *testing.T) {
	c, f := newFakeAPI(t)
	if err := c.SetLabels(context.Background(), 42, map[string]string{"nixzner-status": "ready"}); err != nil {
		t.Fatal(err)
	}
	got := f.updateBody["labels"].(map[string]any)
	if got["nixzner-status"] != "ready" {
		t.Fatalf("update body = %v", f.updateBody)
	}
}

func TestLocations(t *testing.T) {
	c, f := newFakeAPI(t)
	f.locations = set("nbg1", "fsn1")
	got, err := c.Locations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Location{{Name: "fsn1", City: "City of fsn1", Country: "DE"}, {Name: "nbg1", City: "City of nbg1", Country: "DE"}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestServerTypesInStockCheapestFirst(t *testing.T) {
	c, f := newFakeAPI(t)
	f.serverTypes["cpx22"] = "x86"
	f.stock["cpx22"] = map[string]bool{"fsn1": false, "nbg1": true}
	f.prices["cpx22"] = "0.0312"
	got, err := c.ServerTypesInStock(context.Background(), "fsn1")
	if err != nil {
		t.Fatal(err)
	}
	want := []ServerTypeOffer{
		{Name: "cax11", Architecture: "arm", Cores: 2, MemoryGB: 4, HourlyGross: 0.0096},
		{Name: "cx32", Architecture: "x86", Cores: 2, MemoryGB: 4, HourlyGross: 0.0136},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestServerTypesInStockSkipsDeprecated(t *testing.T) {
	c, f := newFakeAPI(t)
	f.deprecated["cx32"] = "fsn1"
	got, err := c.ServerTypesInStock(context.Background(), "fsn1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "cax11" {
		t.Fatalf("got %v, want only cax11", got)
	}
}

func TestSSHKeyNames(t *testing.T) {
	c, f := newFakeAPI(t)
	f.sshKeys = set("zed", "admin")
	got, err := c.SSHKeyNames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"admin", "zed"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
