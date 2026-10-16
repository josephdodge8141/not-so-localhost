package integration

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// localChain keeps browser cookie/redirect semantics while selecting one node.
type localChain struct{}

func (localChain) RoundTrip(r *http.Request) (*http.Response, error) {
	local := r.Clone(r.Context())
	target := *r.URL
	local.URL = &target
	local.Host = r.URL.Host
	local.URL.Host = "127.0.0.1:80"
	local.URL.Scheme = "http"
	local.Header.Set("X-Forwarded-Proto", "https")
	return http.DefaultTransport.RoundTrip(local)
}

func TestLiveAuthorization(t *testing.T) {
	if os.Getenv("NSL_LIVE_TEST") != "1" {
		t.Skip("opt in on the auth owner with NSL_LIVE_TEST=1; creates and disables named fixtures")
	}
	lock, err := net.Listen("tcp", "127.0.0.1:19003")
	if err != nil {
		t.Fatal("another live authorization validation is running")
	}
	defer lock.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{}
	contents, err := os.ReadFile("../../.env")
	must(err)
	for _, line := range strings.Split(string(contents), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && !strings.HasPrefix(key, "#") {
			env[key] = strings.Trim(value, "\"'")
		}
	}
	domain := env["DOMAIN"]
	portal := "https://apps." + domain
	issuer := "https://auth." + domain
	client := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Transport: localChain{}, Jar: jar, Timeout: 25 * time.Second}
	}
	call := func(c *http.Client, method, target string, data any, headers map[string]string) (int, []byte) {
		t.Helper()
		var body io.Reader
		if data != nil {
			encoded, err := json.Marshal(data)
			must(err)
			body = bytes.NewReader(encoded)
		}
		req, err := http.NewRequest(method, target, body)
		must(err)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := c.Do(req)
		must(err)
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		must(err)
		return res.StatusCode, b
	}
	check := func(label string, got, want int) {
		t.Helper()
		if got != want {
			t.Fatalf("%s: got %d want %d", label, got, want)
		}
		t.Log(label, got)
	}
	login := func(username, password string) *http.Client {
		t.Helper()
		c := client()
		res, err := c.Get(portal + "/oauth2/start?rd=" + url.QueryEscape(portal+"/api/v2/me"))
		must(err)
		page, _ := io.ReadAll(res.Body)
		res.Body.Close()
		action := regexp.MustCompile(`<form[^>]*action="([^"]+)"`).FindSubmatch(page)
		if len(action) != 2 {
			t.Fatal("password form absent")
		}
		res, err = c.PostForm(html.UnescapeString(string(action[1])), url.Values{"username": {username}, "password": {password}, "credentialId": {""}})
		must(err)
		res.Body.Close()
		status, body := call(c, "GET", portal+"/api/v2/me", nil, nil)
		check("fresh login", status, 200)
		var me struct {
			User  string
			Admin bool
		}
		if json.Unmarshal(body, &me) != nil || me.User != username || me.Admin != (username == "joe") {
			t.Fatal("wrong browser identity")
		}
		return c
	}
	admin := login("joe", env["KEYCLOAK_USER_PASSWORD"])
	status, _ := call(admin, "GET", portal+"/api/v2/users?q=joe", nil, nil)
	check("user picker", status, 200)
	operator := client()
	res, err := operator.PostForm(issuer+"/realms/master/protocol/openid-connect/token", url.Values{"client_id": {"admin-cli"}, "grant_type": {"password"}, "username": {"admin"}, "password": {env["KEYCLOAK_ADMIN_PASSWORD"]}})
	must(err)
	var token struct {
		AccessToken string `json:"access_token"`
	}
	err = json.NewDecoder(res.Body).Decode(&token)
	res.Body.Close()
	if err != nil || token.AccessToken == "" {
		t.Fatal("operator login failed")
	}
	auth := map[string]string{"Authorization": "Bearer " + token.AccessToken}
	usersURL := issuer + "/admin/realms/local/users"
	status, body := call(operator, "GET", usersURL+"?username=nsl-validation-member&exact=true", nil, auth)
	check("find fixture", status, 200)
	var users []struct{ ID string }
	if json.Unmarshal(body, &users) != nil {
		t.Fatal("invalid users")
	}
	random := make([]byte, 24)
	_, err = rand.Read(random)
	must(err)
	password := hex.EncodeToString(random)
	user := map[string]any{"username": "nsl-validation-member", "firstName": "Validation", "lastName": "Member", "email": "nsl-validation-member@" + domain, "enabled": true, "emailVerified": true, "requiredActions": []string{}}
	if len(users) == 0 {
		status, _ = call(operator, "POST", usersURL, user, auth)
		check("create fixture", status, 201)
		_, body = call(operator, "GET", usersURL+"?username=nsl-validation-member&exact=true", nil, auth)
		if json.Unmarshal(body, &users) != nil || len(users) != 1 {
			t.Fatal("fixture not found")
		}
	}
	userURL := usersURL + "/" + users[0].ID
	status, _ = call(operator, "PUT", userURL, user, auth)
	check("enable fixture", status, 204)
	t.Cleanup(func() {
		status, _ := call(operator, "PUT", userURL, map[string]bool{"enabled": false}, auth)
		check("disable fixture", status, 204)
	})
	status, _ = call(operator, "PUT", userURL+"/reset-password", map[string]any{"type": "password", "value": password, "temporary": false}, auth)
	check("fixture credential", status, 204)
	member := login("nsl-validation-member", password)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1") && r.Header.Get("Authorization") != "Bearer validation-key" {
			w.WriteHeader(401)
		}
		_, _ = w.Write([]byte("validation backend"))
	}))
	defer backend.Close()
	host := "authz-validation--" + env["NODE_NAME"] + "." + domain
	target := strings.Replace(backend.URL, "127.0.0.1", "host.docker.internal", 1)
	input := map[string]any{"name": "authz-validation", "description": "NSL authorization integration fixture", "target_url": target, "grant": map[string]any{"users": []string{users[0].ID}}, "routes": []map[string]any{{"rule": "Host(`" + host + "`) && !(Path(`/v1`) || PathPrefix(`/v1/`))", "auth": "browser", "priority": 100}, {"rule": "Host(`" + host + "`) && (Path(`/v1`) || PathPrefix(`/v1/`))", "auth": "upstream", "priority": 110}}}
	node := &http.Client{Timeout: 25 * time.Second}
	nodeAuth := map[string]string{"Authorization": "Bearer " + env["REGISTRY_API_TOKEN"]}
	appsURL := "http://127.0.0.1:7272/api/v2/apps"
	status, body = call(node, "GET", appsURL, nil, nodeAuth)
	check("local inventory", status, 200)
	type app struct {
		ID, Name, Description string
		Generation            uint64
	}
	var apps []app
	if json.Unmarshal(body, &apps) != nil {
		t.Fatal("invalid apps")
	}
	var fixture app
	for _, a := range apps {
		if a.Name == "authz-validation" {
			fixture = a
		}
	}
	save := func() {
		t.Helper()
		method, endpoint, want := "POST", appsURL, 201
		if fixture.ID != "" {
			method, endpoint, want = "PUT", appsURL+"/"+fixture.ID, 200
			nodeAuth["If-Match"] = fmt.Sprintf(`"app:%s:%d"`, fixture.ID, fixture.Generation)
		}
		status, b := call(admin, method, strings.Replace(endpoint, "http://127.0.0.1:7272", portal, 1), input, map[string]string{"Origin": portal, "If-Match": nodeAuth["If-Match"]})
		check("save fixture", status, want)
		if json.Unmarshal(b, &fixture) != nil {
			t.Fatal("invalid fixture")
		}
	}
	if fixture.ID != "" && fixture.Description != "NSL authorization integration fixture" {
		t.Fatal("fixture name belongs to another app")
	}
	save()
	t.Cleanup(func() { input["enabled"] = false; input["grant"] = map[string]any{"users": []string{}}; save() })
	for attempt := 0; attempt < 30; attempt++ {
		status, _ = call(admin, "GET", "https://"+host+"/", nil, nil)
		if status == 200 {
			break
		}
		time.Sleep(time.Second)
	}
	check("admin app", status, 200)
	for _, tc := range []struct {
		label, target string
		c             *http.Client
		want          int
	}{{"assigned member", "https://" + host + "/", member, 200}, {"member terminal", "https://t--" + env["NODE_NAME"] + "." + domain + "/", member, 403}, {"admin terminal", "https://t--" + env["NODE_NAME"] + "." + domain + "/", admin, 200}, {"member directory", portal + "/api/v2/users", member, 403}} {
		status, _ = call(tc.c, "GET", tc.target, nil, nil)
		check(tc.label, status, tc.want)
	}
	status, _ = call(member, "POST", portal+"/api/v2/apps", input, nil)
	check("member mutation", status, 403)
	status, _ = call(admin, "POST", portal+"/api/v2/apps", input, map[string]string{"Origin": "https://untrusted.example.test"})
	check("cross-origin mutation", status, 403)
	anonymous := client()
	anonymous.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, tc := range []struct {
		label, path string
		headers     map[string]string
		want        int
	}{{"spoofed identity", "/", map[string]string{"X-Auth-Request-User": users[0].ID, "X-Auth-Request-Groups": "admin"}, 401}, {"missing API key", "/v1/models", nil, 401}, {"valid API key", "/v1/models", map[string]string{"Authorization": "Bearer validation-key"}, 200}} {
		status, _ = call(anonymous, "GET", "https://"+host+tc.path, nil, tc.headers)
		check(tc.label, status, tc.want)
	}
	input["grant"] = map[string]any{"users": []string{}}
	save()
	status, _ = call(member, "GET", "https://"+host+"/", nil, nil)
	check("revoked member", status, 403)
	_, body = call(member, "GET", portal+"/api/v2/apps", nil, nil)
	if json.Unmarshal(body, &apps) != nil {
		t.Fatal("invalid inventory")
	}
	for _, a := range apps {
		if a.ID == fixture.ID {
			t.Fatal("revoked app visible")
		}
	}
}
