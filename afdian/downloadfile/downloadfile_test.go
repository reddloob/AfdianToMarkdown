package downloadfile

import (
	"AfdianToMarkdown/afdian"
	"AfdianToMarkdown/config"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testConfig(server, dir string) *config.Config {
	u, _ := url.Parse(server)
	c := config.NewConfig(u.Host, dir, "")
	c.HostUrl = server
	return c
}

func TestRunOrderFilteringLayoutAndRepair(t *testing.T) {
	var server *httptest.Server
	var detailOrder []string
	resourceHits := 0
	stamps := map[string]int64{"pinned": 1000, "new": 4000, "locked": 3000, "empty": 2000}
	fileName := "原名 包 (测试)#1.7z"
	row := func(id string) map[string]any {
		return map[string]any{"post_id": id, "title": id, "publish_time": stamps[id], "publish_sn": id}
	}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "auth_token=test" {
			t.Error("missing authorized cookie")
		}
		var data any
		switch r.URL.Path {
		case "/api/user/get-profile-by-slug":
			data = map[string]any{"user": map[string]any{"user_id": "author-id"}}
		case "/api/post/get-list":
			if r.URL.Query().Get("type") != "new" {
				t.Error("not requesting newest list")
			}
			var rows []map[string]any
			switch r.URL.Query().Get("publish_sn") {
			case "":
				rows = []map[string]any{row("pinned"), row("new")}
			case "new":
				rows = []map[string]any{row("locked"), row("empty"), row("pinned")}
			case "pinned":
				rows = []map[string]any{}
			default:
				t.Error("unexpected pagination cursor")
			}
			data = map[string]any{"list": rows}
		case "/api/post/get-detail":
			id := r.URL.Query().Get("post_id")
			detailOrder = append(detailOrder, id)
			right := 1
			if id == "locked" {
				right = 0
			}
			attachments := []map[string]any{}
			if id != "empty" {
				attachments = append(attachments, map[string]any{"title": fileName, "url": server.URL + "/private-unsigned", "download": server.URL + "/signed", "size": 7})
			}
			data = map[string]any{"post": map[string]any{"post_id": id, "title": id, "has_right": right, "content": "<p>正文内容</p><img src=\"" + server.URL + "/inline.png\"><p>后续文字</p>", "pics": []string{server.URL + "/inline.png", server.URL + "/gallery.png"}, "attachment": attachments}}
		case "/signed":
			http.Redirect(w, r, "/archive", http.StatusFound)
			return
		case "/archive":
			resourceHits++
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("archive"))
			return
		case "/inline.png", "/gallery.png":
			resourceHits++
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("picture"))
			return
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ec": 200, "data": data})
	}))
	defer server.Close()
	cfg := testConfig(server.URL, t.TempDir())
	dir := filepath.Join(cfg.DataDir, "author", "motions")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	stem := time.Unix(4000, 0).Format("2006-01-02_15_04_05") + "_new"
	mdPath := filepath.Join(dir, stem+".md")
	if err := os.WriteFile(mdPath, []byte("old motions export without attachments"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), cfg, "author", "auth_token=test", true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(detailOrder, []string{"new", "locked", "empty", "pinned"}) {
		t.Fatalf("order/dedup: %v", detailOrder)
	}
	if resourceHits != 6 {
		t.Fatalf("expected 2 archives and 4 unique images, got %d", resourceHits)
	}
	markdowns, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	if len(markdowns) != 2 {
		t.Fatalf("got %d Markdown files", len(markdowns))
	}
	b, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"正文内容", "后续文字", "### 附件", localLink(stem, fileName), localLink(stem, "new_0.png"), localLink(stem, "new_1.png")} {
		if !strings.Contains(string(b), s) {
			t.Errorf("Markdown missing %q: %s", s, b)
		}
	}
	if strings.Contains(string(b), "/signed") || strings.Contains(string(b), server.URL+"/inline.png") {
		t.Error("remote asset link remains")
	}
	asset := filepath.Join(dir, ".assets", stem, fileName)
	if b, err := os.ReadFile(asset); err != nil || string(b) != "archive" {
		t.Fatalf("original attachment name not preserved: %s %v", b, err)
	}
	if err := Run(context.Background(), cfg, "author", "auth_token=test", true); err != nil {
		t.Fatal(err)
	}
	if resourceHits != 6 {
		t.Error("complete posts were downloaded again")
	}
	if err := os.Remove(asset); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), cfg, "author", "auth_token=test", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(asset); err != nil {
		t.Fatal("missing asset was not repaired")
	}
}

func TestAPIRejectsLoginFailureAndRepeatedCursor(t *testing.T) {
	for _, mode := range []string{"login", "loop"} {
		t.Run(mode, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "login" {
					fmt.Fprint(w, `{"ec":40100,"em":"该功能需要登录查看"}`)
					return
				}
				if strings.Contains(r.URL.Path, "profile") {
					fmt.Fprint(w, `{"ec":200,"data":{"user":{"user_id":"x"}}}`)
					return
				}
				fmt.Fprint(w, `{"ec":200,"data":{"list":[{"post_id":"p","publish_time":1,"publish_sn":"same"}]}}`)
			}))
			defer s.Close()
			_, err := collectPosts(context.Background(), testConfig(s.URL, t.TempDir()), "author", "")
			if err == nil {
				t.Fatal("expected explicit error, not empty success")
			}
		})
	}
}

func TestDownloadRedirectDoesNotLeakCookie(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("cookie leaked to file host")
		}
		fmt.Fprint(w, "archive")
	}))
	defer cdn.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "auth_token=private" {
			t.Error("origin did not receive cookie")
		}
		http.Redirect(w, r, cdn.URL+"/file", http.StatusFound)
	}))
	defer origin.Close()
	err := downloadFile(context.Background(), origin.URL+"/redirect?signature=secret", filepath.Join(t.TempDir(), "x.7z"), origin.URL, origin.URL+"/p/id", "auth_token=private", 7)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDownloadFailureLeavesNoCompletedFile(t *testing.T) {
	for _, mode := range []string{"forbidden", "html", "short", "retry"} {
		t.Run(mode, func(t *testing.T) {
			hits := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				switch mode {
				case "forbidden":
					w.WriteHeader(403)
				case "html":
					w.Header().Set("Content-Type", "text/html")
					fmt.Fprint(w, "<html>login</html>")
				case "short":
					fmt.Fprint(w, "bad")
				case "retry":
					if hits == 1 {
						w.WriteHeader(503)
					} else {
						fmt.Fprint(w, "archive")
					}
				}
			}))
			defer s.Close()
			dir := t.TempDir()
			dest := filepath.Join(dir, "x.7z")
			err := downloadFile(context.Background(), s.URL+"/?signature=TOPSECRET", dest, s.URL, s.URL, "", 7)
			if mode == "retry" {
				if err != nil || hits != 2 {
					t.Fatalf("retry failed: %v, %d", err, hits)
				}
				return
			}
			if err == nil {
				t.Fatal("expected failure")
			}
			if strings.Contains(err.Error(), "TOPSECRET") {
				t.Error("signed URL leaked in error")
			}
			files, _ := os.ReadDir(dir)
			if len(files) != 0 {
				t.Fatalf("partial/final files remain: %v", files)
			}
			if (mode == "forbidden" || mode == "html") && hits != 1 {
				t.Error("permanent error retried")
			}
		})
	}
}

func TestFailedPostCanResume(t *testing.T) {
	fail := true
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(403)
			return
		}
		fmt.Fprint(w, "archive")
	}))
	defer s.Close()
	cfg := testConfig(s.URL, t.TempDir())
	d := postDetail{ID: "p", Title: "帖子", Attachments: []attachment{{Name: "mod.7z", URL: s.URL, Size: 7}}}
	a := afdian.Post{Url: s.URL + "/p/p", PublishTime: time.Unix(1000, 0)}
	if _, err := savePost(context.Background(), cfg, cfg.DataDir, a, d, "", true); err == nil {
		t.Fatal("failure ignored")
	}
	files, _ := filepath.Glob(filepath.Join(cfg.DataDir, "*.md"))
	if len(files) != 0 {
		t.Fatal("failed post marked complete")
	}
	fail = false
	if _, err := savePost(context.Background(), cfg, cfg.DataDir, a, d, "", true); err != nil {
		t.Fatal(err)
	}
	if done, err := savePost(context.Background(), cfg, cfg.DataDir, a, d, "", true); err != nil || !done {
		t.Fatalf("completion not recognized: %v", err)
	}
}

func TestFilenamesAndCancellation(t *testing.T) {
	link := localLink("新 帖 (测试)#1", "原名 [文件](1)%.7z")
	decoded, err := url.PathUnescape(link)
	if err != nil || decoded != ".assets/新 帖 (测试)#1/原名 [文件](1)%.7z" || strings.ContainsAny(link, " ()#[]") {
		t.Fatalf("unsafe Markdown link: %s", link)
	}
	for input, want := range map[string]string{"原名 (1).7z": "原名 (1).7z", "../../x.zip": ".._.._x.zip", "CON.zip": "_CON.zip", "a:b?.zip": "a_b_.zip", "tail. ": "tail", "\x00.zip": "_.zip"} {
		if got := safeName(input); got != want {
			t.Errorf("%q -> %q, want %q", input, got, want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, config.NewConfig("example.com", t.TempDir(), ""), "author", "", true); err != context.Canceled {
		t.Fatalf("cancellation lost: %v", err)
	}
	if err := Run(context.Background(), config.NewConfig("example.com", t.TempDir(), ""), "../escape", "", true); err == nil {
		t.Fatal("unsafe author path accepted")
	}
}

// Opt-in smoke test of one authorized post; never crawls the author's full feed.
func TestLiveSingleAttachmentPost(t *testing.T) {
	cookiePath := os.Getenv("AFDIAN_LIVE_COOKIE")
	if cookiePath == "" {
		t.Skip("set AFDIAN_LIVE_COOKIE for one-post live verification")
	}
	dir := os.Getenv("AFDIAN_LIVE_OUTPUT")
	if dir == "" {
		dir = t.TempDir()
	}
	cookie, _, err := afdian.GetCookies(cookiePath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfig("ifdian.net", dir, cookiePath)
	a := afdian.Post{Url: "https://ifdian.net/p/208c59f4b6b511f1b1025254001e7c00"}
	d, err := getDetail(context.Background(), cfg, a, cookie)
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasRight || len(d.Attachments) == 0 {
		t.Fatal("no accessible attachments")
	}
	// Get the publication timestamp from the same API for the real filename.
	data, err := api(context.Background(), cfg, "/api/post/get-detail", url.Values{"post_id": {d.ID}}, cookie, a.Url)
	if err != nil {
		t.Fatal(err)
	}
	a.PublishTime = time.Unix(data.Get("post.publish_time").Int(), 0)
	if _, err := savePost(context.Background(), cfg, filepath.Join(dir, "aqechoo", "motions"), a, d, cookie, true); err != nil {
		t.Fatal(err)
	}
}
