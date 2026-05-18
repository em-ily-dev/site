// Command site generates the static pages served at ily.dev.
//
// Its job is to emit go-import meta tags so that `go install ily.dev/foo`
// resolves to the corresponding repository under github.com/em-ily-dev.
//
// On each run it:
//   - enumerates public, non-fork, non-archived repos in the GitHub account,
//   - fetches go.mod from the root of the default branch,
//   - keeps repos whose module path lives under the configured domain, and
//   - writes a static HTML page per module plus a top-level index.
//
// Set GITHUB_TOKEN to lift the unauthenticated 60-req/hr rate limit.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	githubUser = flag.String("user", "em-ily-dev,act-three", "comma-separated GitHub users or orgs to scan")
	domain     = flag.String("domain", "ily.dev", "vanity import domain")
	outputDir  = flag.String("output", "public", "output directory")
)

type repo struct {
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
	Description   string `json:"description"`
	Fork          bool   `json:"fork"`
	Archived      bool   `json:"archived"`
	Private       bool   `json:"private"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

type module struct {
	Path         string // full import path, e.g. "ily.dev/foo"
	Suffix       string // path under the domain, e.g. "foo"
	CanonicalURL string // e.g. "https://ily.dev/foo/" -- the module's identity
	GodocURL     string // e.g. "https://pkg.go.dev/ily.dev/foo" -- human landing page
	RepoURL      string // e.g. "https://github.com/em-ily-dev/foo" -- current source host
	Branch       string // default branch for go-source links
	Description  string
}

func main() {
	flag.Parse()
	log.SetFlags(0)
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var repos []repo
	for account := range strings.SplitSeq(*githubUser, ",") {
		account = strings.TrimSpace(account)
		batch, err := listRepos(account)
		if err != nil {
			return fmt.Errorf("listing repos for %s: %w", account, err)
		}
		log.Printf("scanned %d public repos for %s", len(batch), account)
		repos = append(repos, batch...)
	}

	prefix := *domain + "/"
	seen := map[string]string{} // module path -> repo name (for dup detection)
	var modules []module

	for _, r := range repos {
		// Archived repos are kept on purpose: people may already depend on
		// those module paths and `go install` should keep resolving them.
		if r.Fork || r.Private {
			continue
		}
		modPath, err := fetchModulePath(r)
		if err != nil {
			log.Printf("  %s: skip (%v)", r.Name, err)
			continue
		}
		if modPath == "" || !strings.HasPrefix(modPath, prefix) {
			continue
		}
		if prev, ok := seen[modPath]; ok {
			log.Printf("  %s: duplicate module path %q (also in %s); skipping", r.Name, modPath, prev)
			continue
		}
		seen[modPath] = r.Name
		suffix := strings.TrimPrefix(modPath, prefix)
		modules = append(modules, module{
			Path:         modPath,
			Suffix:       suffix,
			CanonicalURL: fmt.Sprintf("https://%s/%s/", *domain, suffix),
			GodocURL:     "https://pkg.go.dev/" + modPath,
			RepoURL:      r.HTMLURL,
			Branch:       r.DefaultBranch,
			Description:  r.Description,
		})
		log.Printf("  %s: publish %s", r.Name, modPath)
	}

	sort.Slice(modules, func(i, j int) bool { return modules[i].Path < modules[j].Path })

	if err := os.RemoveAll(*outputDir); err != nil {
		return err
	}
	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		return err
	}
	for _, m := range modules {
		if err := writeModulePage(m); err != nil {
			return fmt.Errorf("writing %s: %w", m.Path, err)
		}
	}
	if err := writeIndex(modules); err != nil {
		return fmt.Errorf("writing index: %w", err)
	}
	// CNAME tells GitHub Pages which custom domain to serve from. Harmless
	// on hosts that ignore it.
	cname := filepath.Join(*outputDir, "CNAME")
	if err := os.WriteFile(cname, []byte(*domain+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing CNAME: %w", err)
	}

	log.Printf("wrote %d module pages to %s", len(modules), *outputDir)
	return nil
}

// --- GitHub ---------------------------------------------------------------

var httpClient = &http.Client{Timeout: 30 * time.Second}

func ghRequest(method, url string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ily.dev-ssg")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return httpClient.Do(req)
}

func listRepos(user string) ([]repo, error) {
	var all []repo
	for page := 1; ; page++ {
		url := fmt.Sprintf("https://api.github.com/users/%s/repos?type=public&per_page=100&page=%d", user, page)
		resp, err := ghRequest("GET", url)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("github api %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		}
		var batch []repo
		err = json.NewDecoder(resp.Body).Decode(&batch)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			return all, nil
		}
	}
}

func fetchModulePath(r repo) (string, error) {
	branch := r.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/go.mod", r.Owner.Login, r.Name, branch)
	resp, err := httpClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return parseModulePath(resp.Body)
	case http.StatusNotFound:
		return "", nil // no go.mod -- not a Go module
	default:
		return "", fmt.Errorf("fetching go.mod: status %d", resp.StatusCode)
	}
}

// parseModulePath returns the value of the `module` directive in a go.mod
// stream. Handles both the single-line form (`module foo`) and the block form
// (`module ( "foo" )`), as well as quoted paths and trailing line comments.
func parseModulePath(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := stripComment(strings.TrimSpace(sc.Text()))
		if line == "" || !strings.HasPrefix(line, "module") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "module"))
		if rest != "(" {
			return unquote(rest), nil
		}
		for sc.Scan() {
			inner := stripComment(strings.TrimSpace(sc.Text()))
			if inner == "" {
				continue
			}
			if inner == ")" {
				return "", nil
			}
			return unquote(inner), nil
		}
	}
	return "", sc.Err()
}

func stripComment(s string) string {
	if before, _, ok := strings.Cut(s, "//"); ok {
		return strings.TrimSpace(before)
	}
	return s
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// --- output ---------------------------------------------------------------

// modulePageTmpl emits the go-import (and go-source) meta tags Go's vanity
// import resolver looks for, plus a meta-refresh so a human visiting the URL
// in a browser lands on the GitHub repo.
var modulePageTmpl = template.Must(template.New("module").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>{{.Path}}</title>
<meta name="go-import" content="{{.Path}} git {{.RepoURL}}">
<meta name="go-source" content="{{.Path}} {{.RepoURL}} {{.RepoURL}}/tree/{{.Branch}}{/dir} {{.RepoURL}}/blob/{{.Branch}}{/dir}/{file}#L{line}">
<meta http-equiv="refresh" content="0; url={{.GodocURL}}">
<link rel="canonical" href="{{.CanonicalURL}}">
</head>
<body>
<p>Go module <code>{{.Path}}</code> &mdash; docs at <a href="{{.GodocURL}}">{{.GodocURL}}</a>, source at <a href="{{.RepoURL}}">{{.RepoURL}}</a>.</p>
</body>
</html>
`))

func writeModulePage(m module) error {
	dir := filepath.Join(*outputDir, filepath.FromSlash(m.Suffix))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "index.html"))
	if err != nil {
		return err
	}
	defer f.Close()
	return modulePageTmpl.Execute(f, m)
}

var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>{{.Domain}}</title>
</head>
<body>
<h1>{{.Domain}}</h1>
{{if .Modules}}<ul>
{{range .Modules}}<li><a href="/{{.Suffix}}/"><code>{{.Path}}</code></a>{{with .Description}} &mdash; {{.}}{{end}}</li>
{{end}}</ul>{{else}}<p>No modules published yet.</p>{{end}}
</body>
</html>
`))

func writeIndex(mods []module) error {
	f, err := os.Create(filepath.Join(*outputDir, "index.html"))
	if err != nil {
		return err
	}
	defer f.Close()
	return indexTmpl.Execute(f, struct {
		Domain  string
		Modules []module
	}{*domain, mods})
}
