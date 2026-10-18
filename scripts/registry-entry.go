//go:build ignore

// registry-entry writes a plugin's entry for a Code Foundry plugin registry
// (plugins/<name>.json in alexwaumann/code-foundry-plugins) from plugin.json and the
// release's SHA256SUMS: name, display name, description, repository, version, protocol,
// all eight permission keys, and the darwin/arm64 and darwin/amd64 tarball URLs of the
// GitHub release v<version> with their hashes. `make registry-entry` runs it:
//
//	go run scripts/registry-entry.go -repo owner/name -sums dist/SHA256SUMS.release -out dist/<name>.json
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/alexwaumann/code-foundry/pluginsdk"
)

// permissions is pluginsdk.Permissions with every key written, as the registry wants.
type permissions struct {
	Commands     []string `json:"commands"`
	Events       []string `json:"events"`
	ThreadStatus bool     `json:"threadStatus"`
	SessionEnv   bool     `json:"sessionEnv"`
	Secrets      bool     `json:"secrets"`
	Threads      bool     `json:"threads"`
	Terminals    bool     `json:"terminals"`
	API          bool     `json:"api"`
}

type artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type entry struct {
	Name        string              `json:"name"`
	DisplayName string              `json:"displayName"`
	Description string              `json:"description"`
	Repo        string              `json:"repo"`
	Version     string              `json:"version"`
	Protocol    int                 `json:"protocol"`
	Permissions permissions         `json:"permissions"`
	Artifacts   map[string]artifact `json:"artifacts"`
}

func main() {
	manifest := flag.String("manifest", "plugin.json", "the plugin's manifest")
	sums := flag.String("sums", "", "the release's SHA256SUMS (required)")
	repo := flag.String("repo", "", "the plugin's GitHub repository, owner/name (required)")
	out := flag.String("out", "", "where to write the entry (default stdout)")
	flag.Parse()
	if err := run(*manifest, *sums, *repo, *out); err != nil {
		fmt.Fprintln(os.Stderr, "registry-entry:", err)
		os.Exit(1)
	}
}

func run(manifestPath, sumsPath, repo, out string) error {
	if sumsPath == "" || !strings.Contains(repo, "/") {
		return errors.New("-sums and -repo owner/name are required")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	m, _, err := pluginsdk.LoadManifest(data)
	if err != nil {
		return err
	}
	hashes, err := readSums(sumsPath)
	if err != nil {
		return err
	}
	p := m.Permissions.Normalize()
	e := entry{
		Name: m.Name, DisplayName: m.DisplayNameOr(), Description: m.Description, Repo: repo,
		Version: m.Version, Protocol: m.Protocol,
		Permissions: permissions{Commands: orEmpty(p.Commands), Events: orEmpty(p.Events), ThreadStatus: p.ThreadStatus,
			SessionEnv: p.SessionEnv, Secrets: p.Secrets, Threads: p.Threads, Terminals: p.Terminals, API: p.API},
		Artifacts: map[string]artifact{},
	}
	for _, arch := range []string{"arm64", "amd64"} {
		file := fmt.Sprintf("%s-%s-darwin-%s.tar.gz", m.Name, m.Version, arch)
		sum, ok := hashes[file]
		if !ok {
			return fmt.Errorf("%s has no line for %s (is it the release v%s's?)", sumsPath, file, m.Version)
		}
		e.Artifacts["darwin/"+arch] = artifact{
			URL:    fmt.Sprintf("https://github.com/%s/releases/download/v%s/%s", repo, m.Version, file),
			SHA256: sum,
		}
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if out == "" {
		_, err = os.Stdout.Write(b)
		return err
	}
	return os.WriteFile(out, b, 0o644)
}

// readSums reads `shasum -a 256` output: "<hex>  <file>" per line.
func readSums(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && len(fields[0]) == 64 {
			out[strings.TrimPrefix(fields[1], "*")] = fields[0]
		}
	}
	return out, sc.Err()
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
