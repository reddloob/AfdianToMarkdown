// Package downloadfile saves accessible posts with platform-hosted attachments.
package downloadfile

import (
	"AfdianToMarkdown/afdian"
	"AfdianToMarkdown/config"
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"golang.org/x/exp/slog"
)

type attachment struct {
	Name string
	URL  string
	Size int64
}

type postDetail struct {
	ID          string
	Title       string
	Content     string
	Pictures    []string
	Attachments []attachment
	HasRight    bool
	PublishTime time.Time
}

func api(ctx context.Context, cfg *config.Config, endpoint string, query url.Values, cookie, referer string) (gjson.Result, error) {
	if err := ctx.Err(); err != nil {
		return gjson.Result{}, err
	}
	body, err := afdian.NewRequestGet(cfg.Host, cfg.HostUrl+endpoint+"?"+query.Encode(), cookie, referer)
	if err != nil {
		return gjson.Result{}, err
	}
	if !gjson.ValidBytes(body) {
		return gjson.Result{}, fmt.Errorf("%s: 返回的不是 JSON（可能需要重新登录）", endpoint)
	}
	response := gjson.ParseBytes(body)
	if response.Get("ec").Int() != 200 {
		return gjson.Result{}, fmt.Errorf("%s: API 错误 %s: %s", endpoint, response.Get("ec").String(), response.Get("em").String())
	}
	return response.Get("data"), nil
}

// collectPosts sorts the complete list: pinned posts may cross page boundaries.
func collectPosts(ctx context.Context, cfg *config.Config, author, cookie string) ([]afdian.Post, error) {
	referer := cfg.HostUrl + "/a/" + url.PathEscape(author) + "?tab=feed"
	profile, err := api(ctx, cfg, "/api/user/get-profile-by-slug", url.Values{"url_slug": {author}}, cookie, referer)
	if err != nil {
		return nil, err
	}
	id := profile.Get("user.user_id").String()
	if id == "" {
		return nil, fmt.Errorf("未找到作者 %q", author)
	}
	var posts []afdian.Post
	seenPosts, seenCursors := map[string]bool{}, map[string]bool{}
	cursor := ""
	for {
		data, err := api(ctx, cfg, "/api/post/get-list", url.Values{
			"user_id": {id}, "type": {"new"}, "publish_sn": {cursor}, "per_page": {"10"},
			"group_id": {""}, "all": {"1"}, "is_public": {""}, "plan_id": {""}, "title": {""}, "name": {""},
		}, cookie, referer)
		if err != nil {
			return nil, err
		}
		list := data.Get("list")
		if !list.IsArray() {
			return nil, fmt.Errorf("帖子列表缺少 data.list")
		}
		rows := list.Array()
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			postID := row.Get("post_id").String()
			if postID == "" || row.Get("publish_time").Int() <= 0 {
				return nil, fmt.Errorf("帖子缺少 ID 或发布时间")
			}
			if seenPosts[postID] {
				continue
			}
			seenPosts[postID] = true
			posts = append(posts, afdian.Post{Name: row.Get("title").String(), Url: cfg.HostUrl + "/p/" + url.PathEscape(postID), PublishTime: time.Unix(row.Get("publish_time").Int(), 0)})
		}
		slog.Info("已收集帖子", "count", len(posts))
		next := rows[len(rows)-1].Get("publish_sn").String()
		if next == "" {
			break
		}
		if next == cursor || seenCursors[next] {
			return nil, fmt.Errorf("帖子分页游标重复，停止以避免死循环")
		}
		seenCursors[next], cursor = true, next
		if err := pause(ctx, time.Duration(afdian.DelayMs)*time.Millisecond); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(posts, func(i, j int) bool { return posts[i].PublishTime.After(posts[j].PublishTime) })
	return posts, nil
}

func getDetail(ctx context.Context, cfg *config.Config, article afdian.Post, cookie string) (postDetail, error) {
	parts := strings.Split(strings.TrimRight(article.Url, "/"), "/")
	id := parts[len(parts)-1]
	data, err := api(ctx, cfg, "/api/post/get-detail", url.Values{"post_id": {id}, "album_id": {""}}, cookie, article.Url)
	if err != nil {
		return postDetail{}, err
	}
	p := data.Get("post")
	if !p.IsObject() || p.Get("post_id").String() != id {
		return postDetail{}, fmt.Errorf("帖子详情缺失或 ID 不匹配: %s", id)
	}
	right := p.Get("has_right")
	d := postDetail{ID: id, Title: p.Get("title").String(), Content: p.Get("content").String(), PublishTime: article.PublishTime,
		HasRight: right.Bool() || right.Int() == 1}
	if !d.HasRight {
		return d, nil
	}
	for _, pic := range p.Get("pics").Array() {
		if pic.String() != "" {
			d.Pictures = append(d.Pictures, pic.String())
		}
	}
	for _, item := range p.Get("attachment").Array() {
		// download is the signed redirect; url may point to an inaccessible private object.
		link := item.Get("download").String()
		if link == "" {
			link = item.Get("url").String()
		}
		if link == "" {
			continue
		}
		d.Attachments = append(d.Attachments, attachment{Name: item.Get("title").String(), URL: link, Size: item.Get("size").Int()})
	}
	return d, nil
}

func pause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
