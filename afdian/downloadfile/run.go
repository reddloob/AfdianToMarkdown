package downloadfile

import (
	"AfdianToMarkdown/afdian"
	"AfdianToMarkdown/config"
	"AfdianToMarkdown/utils"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/PuerkitoBio/goquery"
	"golang.org/x/exp/slog"
)

const manifestName = ".downloadfile.json"

type manifest struct {
	Version int              `json:"version"`
	PostID  string           `json:"post_id"`
	Files   map[string]int64 `json:"files"`
}

// Run checks details in descending publication order. Existing motions are upgraded
// when they lack a downloadfile completion manifest; a Markdown alone is not proof
// that attachments have been saved.
func Run(ctx context.Context, cfg *config.Config, author, cookie string, disableComment bool) error {
	if author == "" || author == "." || author == ".." || safeName(author) != author {
		return fmt.Errorf("-au 必须是作者主页 /a/ 后的 ID，不能是路径或完整网址")
	}
	slog.Info("收集作者动态，按发布时间从新到旧排序", "author", author)
	posts, err := collectPosts(ctx, cfg, author, cookie)
	if err != nil {
		return err
	}
	slog.Info("帖子列表已就绪", "count", len(posts))
	dir := filepath.Join(cfg.DataDir, author, "motions")
	saved, skipped, failed := 0, 0, 0
	for i, article := range posts {
		if err := ctx.Err(); err != nil {
			return err
		}
		slog.Info("检查帖子", "index", i+1, "total", len(posts), "title", article.Name)
		detail, err := getDetail(ctx, cfg, article, cookie)
		if err == nil && (!detail.HasRight || len(detail.Attachments) == 0) {
			slog.Info("跳过帖子：无访问权限或无可下载附件", "title", article.Name)
			skipped++
		} else {
			var existing bool
			if err == nil {
				existing, err = savePost(ctx, cfg, dir, article, detail, cookie, disableComment)
			}
			if err != nil {
				failed++
				if e := cfg.HandleErr(err, "下载附件帖子失败", "title", article.Name); e != nil {
					return e
				}
			} else if existing {
				skipped++
			} else {
				saved++
			}
		}
		if err := pause(ctx, time.Duration(afdian.DelayMs)*time.Millisecond); err != nil {
			return err
		}
	}
	slog.Info("附件帖子处理完毕", "saved", saved, "skipped", skipped, "failed", failed)
	if failed > 0 {
		return fmt.Errorf("%d 篇帖子下载失败，请重新运行 downloadfile 重试", failed)
	}
	return nil
}

// safeName preserves original names except characters Windows cannot store.
func safeName(name string) string {
	name = utils.ToSafeFilename(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimRight(name, ". ")
	if name == "" {
		name = "untitled"
	}
	base := strings.ToUpper(strings.TrimRight(strings.SplitN(name, ".", 2)[0], " "))
	runes := []rune(base)
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
		(len(runes) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && strings.ContainsRune("123456789¹²³", runes[3])) {
		name = "_" + name
	}
	return name
}

func localLink(stem, name string) string {
	// Escape spaces, parentheses, '#' and '%' so Markdown resolves local files correctly.
	escaped := (&url.URL{Path: path.Join(utils.ImgDir, stem, name)}).EscapedPath()
	return strings.NewReplacer("(", "%28", ")", "%29").Replace(escaped)
}

func label(s string) string {
	return strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]", "\n", " ", "\r", " ").Replace(s)
}

func complete(mdPath, assetsDir, id string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(assetsDir, manifestName))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var m manifest
	if json.Unmarshal(b, &m) != nil || m.Version != 1 {
		return false, nil
	}
	if m.PostID != id {
		return false, fmt.Errorf("同名 Markdown 已属于另一篇帖子，停止以免覆盖: %s", mdPath)
	}
	if info, err := os.Stat(mdPath); err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return false, nil
	}
	if len(m.Files) == 0 {
		return false, nil
	}
	for name, size := range m.Files {
		if safeName(name) != name {
			return false, nil
		}
		info, err := os.Stat(filepath.Join(assetsDir, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() != size {
			return false, nil
		}
	}
	return true, nil
}

func savePost(ctx context.Context, cfg *config.Config, dir string, article afdian.Post, d postDetail, cookie string, disableComment bool) (bool, error) {
	stem := article.PublishTime.Format("2006-01-02_15_04_05") + "_" + safeName(d.Title)
	mdPath := filepath.Join(dir, stem+".md")
	assetsDir := filepath.Join(dir, utils.ImgDir, stem)
	if done, err := complete(mdPath, assetsDir, d.ID); done || err != nil {
		return done, err
	}
	if err := os.MkdirAll(assetsDir, 0755); err != nil {
		return false, err
	}
	// Remove a stale completion marker before attempting repairs.
	if err := os.Remove(filepath.Join(assetsDir, manifestName)); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	m := manifest{Version: 1, PostID: d.ID, Files: map[string]int64{}}
	used := map[string]bool{strings.ToLower(manifestName): true}
	fileNames := make([]string, len(d.Attachments))
	for i, a := range d.Attachments {
		name := a.Name
		if name == "" {
			if u, err := resolveHTTP(article.Url, a.URL); err == nil {
				name = path.Base(u.Path)
			}
		}
		name = safeName(name)
		if used[strings.ToLower(name)] {
			return false, fmt.Errorf("同一帖子附件文件名重复或冲突: %s", name)
		}
		used[strings.ToLower(name)] = true
		fileNames[i] = name
	}
	store := func(raw, name string, size int64) error {
		destination := filepath.Join(assetsDir, name)
		// Only reuse partial-run files when the API supplies a verifiable size.
		info, err := os.Stat(destination)
		if err != nil || !info.Mode().IsRegular() || size <= 0 || info.Size() != size {
			slog.Info("下载资源", "post", d.Title, "file", name)
			if err := downloadFile(ctx, raw, destination, cfg.HostUrl, article.Url, cookie, size); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		info, err = os.Stat(destination)
		if err != nil {
			return err
		}
		m.Files[name] = info.Size()
		return nil
	}
	var attachments strings.Builder
	for i, a := range d.Attachments {
		if err := store(a.URL, fileNames[i], a.Size); err != nil {
			return false, err
		}
		fmt.Fprintf(&attachments, "- [%s](<%s>)\n", label(fileNames[i]), localLink(stem, fileNames[i]))
	}
	imageLinks := map[string]string{}
	imageIndex := 0
	image := func(raw string) (string, error) {
		u, err := resolveHTTP(article.Url, raw)
		if err != nil {
			return "", err
		}
		key := u.String()
		if local, ok := imageLinks[key]; ok {
			return local, nil
		}
		ext := path.Ext(u.Path)
		if ext == "" || len(ext) > 10 {
			ext = ".jpg"
		}
		var name string
		for {
			name = safeName(fmt.Sprintf("%s_%d%s", d.Title, imageIndex, ext))
			imageIndex++
			if !used[strings.ToLower(name)] {
				break
			}
		}
		used[strings.ToLower(name)] = true
		if err := store(key, name, 0); err != nil {
			return "", err
		}
		local := localLink(stem, name)
		imageLinks[key] = local
		return local, nil
	}
	// Rewrite inline images before conversion to preserve their positions in the text.
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(d.Content))
	if err != nil {
		return false, err
	}
	var imageErr error
	doc.Find("img").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		raw, ok := s.Attr("src")
		if !ok || raw == "" {
			return true
		}
		local, err := image(raw)
		if err != nil {
			imageErr = err
			return false
		}
		s.SetAttr("src", local)
		return true
	})
	if imageErr != nil {
		return false, imageErr
	}
	inlineImages := map[string]bool{}
	for key := range imageLinks {
		inlineImages[key] = true
	}
	var pictures strings.Builder
	for _, raw := range d.Pictures {
		u, err := resolveHTTP(article.Url, raw)
		if err != nil {
			return false, err
		}
		if inlineImages[u.String()] {
			continue
		}
		local, err := image(raw)
		if err != nil {
			return false, err
		}
		fmt.Fprintf(&pictures, "![image](<%s>)\n\n", local)
		inlineImages[u.String()] = true
	}
	htmlContent, err := doc.Find("body").Html()
	if err != nil {
		return false, err
	}
	content, err := md.NewConverter("", true, nil).ConvertString(htmlContent)
	if err != nil {
		return false, err
	}
	articleContent := fmt.Sprintf("## %s\n\n### Refer\n\n%s\n\n### 正文\n\n%s\n\n%s\n### 附件\n\n%s", d.Title, article.Url, content, pictures.String(), attachments.String())
	if !disableComment {
		comments, hot, err := afdian.GetPostComment(cfg, article.Url, cookie)
		if err != nil {
			return false, err
		}
		articleContent += "\n\n" + hot + "\n\n" + comments
	}
	if err := atomicWrite(mdPath, []byte(articleContent)); err != nil {
		return false, err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return false, err
	}
	if err := atomicWrite(filepath.Join(assetsDir, manifestName), b); err != nil {
		return false, err
	}
	slog.Info("帖子及附件已保存", "path", mdPath)
	return false, nil
}

func atomicWrite(destination string, body []byte) error {
	f, err := os.CreateTemp(filepath.Dir(destination), ".post-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, writeErr := f.Write(body)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, destination)
}
