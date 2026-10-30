package github

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxArtifactRuns     = 5
	maxArtifactsPerRun  = 20
	maxArtifactArchive  = 8 << 20
	maxArtifactExpanded = 16 << 20
	maxArtifactEntries  = 200
)

// RunArtifacts contains workflow-wide evidence, not a job-to-artifact mapping.
// GitHub does not identify the uploading job in the artifact listing.
type RunArtifacts struct {
	RunID         int64      `json:"run_id"`
	RunAttempt    int        `json:"run_attempt"`
	HeadOID       string     `json:"head_oid"`
	Artifacts     []Artifact `json:"artifacts"`
	EvidenceError string     `json:"evidence_error,omitempty"`
}

type Artifact struct {
	ID              int64          `json:"id"`
	Name            string         `json:"name"`
	URL             string         `json:"url"`
	Size            int64          `json:"size_in_bytes"`
	Expired         bool           `json:"expired"`
	DownloadCommand string         `json:"download_command,omitempty"`
	Files           []ArtifactFile `json:"files"`
	EvidenceError   string         `json:"evidence_error,omitempty"`
}

type ArtifactFile struct {
	Path          string `json:"path"`
	Content       string `json:"content,omitempty"`
	Truncated     bool   `json:"truncated,omitempty"`
	EvidenceError string `json:"evidence_error,omitempty"`
}

type artifactBudget struct{ runs, download, expanded, text, files int }

func newArtifactBudget() *artifactBudget {
	return &artifactBudget{maxArtifactRuns, 3 * maxArtifactArchive, 2 * maxArtifactExpanded, 3 * maxEvidenceBytes, maxArtifactEntries}
}

// Artifact download redirects use a fresh header set. Signed storage URLs and
// their errors must never appear in reports or receive GitHub credentials.
func artifactRedirect(req *http.Request, via []*http.Request) error {
	host := strings.ToLower(req.URL.Hostname())
	if len(via) > 3 || req.URL.Scheme != "https" || req.URL.User != nil || req.URL.Port() != "" ||
		(!strings.HasSuffix(host, ".blob.core.windows.net") && !strings.HasSuffix(host, ".githubusercontent.com")) {
		return fmt.Errorf("unsupported artifact download redirect")
	}
	req.Header = make(http.Header)
	return nil
}

type listedArtifact struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Size      *int64    `json:"size_in_bytes"`
	Expired   *bool     `json:"expired"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Run       *struct {
		ID      int64  `json:"id"`
		HeadSHA string `json:"head_sha"`
	} `json:"workflow_run"`
}

func (c *prClient) runArtifacts(ctx context.Context, target PRTarget, run ActionsRun, budget *artifactBudget) RunArtifacts {
	result := RunArtifacts{RunID: run.ID, RunAttempt: run.Attempt, HeadOID: run.HeadSHA, Artifacts: []Artifact{}}
	if budget.runs <= 0 {
		result.EvidenceError = "artifact workflow budget exhausted"
		return result
	}
	budget.runs--
	// Use the attempt endpoint explicitly: a workflow run's original start
	// cannot establish whether an artifact predates a rerun.
	var attempt ActionsRun
	err := c.rest.DoWithContext(ctx, "GET", fmt.Sprintf("repos/%s/%s/actions/runs/%d/attempts/%d", target.Owner, target.Repo, run.ID, run.Attempt), nil, &attempt)
	if err != nil || attempt.ID != run.ID || attempt.Attempt != run.Attempt || attempt.HeadSHA != run.HeadSHA || attempt.SuiteID != run.SuiteID ||
		attempt.WorkflowID != run.WorkflowID || attempt.Event != run.Event || attempt.Status != "completed" || attempt.Conclusion != run.Conclusion ||
		!strings.EqualFold(attempt.Repository.FullName, run.Repository.FullName) {
		result.EvidenceError = "artifact attempt identity unavailable or mismatched"
		return result
	}
	started, startErr := time.Parse(time.RFC3339, attempt.StartedAt)
	updated, endErr := time.Parse(time.RFC3339, attempt.UpdatedAt)
	if startErr != nil || endErr != nil || updated.Before(started) {
		result.EvidenceError = "artifact attempt timestamps unavailable; cannot exclude earlier attempts"
		return result
	}
	endpoint := fmt.Sprintf("repos/%s/%s/actions/runs/%d/artifacts?per_page=%d", target.Owner, target.Repo, run.ID, maxArtifactsPerRun)
	resp, err := c.rest.RequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		result.EvidenceError = "artifact listing unavailable: " + err.Error()
		return result
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	resp.Body.Close()
	var list struct {
		Total     *int              `json:"total_count"`
		Artifacts []*listedArtifact `json:"artifacts"`
	}
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, &list) != nil || list.Total == nil || list.Artifacts == nil ||
		*list.Total < len(list.Artifacts) || len(list.Artifacts) > maxArtifactsPerRun {
		result.EvidenceError = "artifact listing incomplete or exceeds metadata limit"
		return result
	}
	seen := make(map[int64]bool)
	for _, a := range list.Artifacts {
		if a == nil || a.ID <= 0 || strings.TrimSpace(a.Name) == "" || len(a.Name) > 256 || strings.ContainsAny(a.Name, "\x00\r\n") || a.Size == nil || *a.Size < 0 || a.Expired == nil || seen[a.ID] {
			result.EvidenceError = "artifact listing contains missing or duplicate identities"
			return result
		}
		seen[a.ID] = true
	}
	if *list.Total != len(list.Artifacts) {
		result.EvidenceError = "artifact listing truncated at 20 entries; additional artifacts were not inspected"
	}
	if *list.Total == 0 {
		result.EvidenceError = "no workflow artifacts available"
	}
	for _, a := range list.Artifacts {
		artifact := Artifact{ID: a.ID, Name: a.Name, Size: *a.Size, Expired: *a.Expired, Files: []ArtifactFile{},
			URL: fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d/artifacts/%d", target.Owner, target.Repo, run.ID, a.ID)}
		switch {
		case a.Run == nil || a.Run.ID != run.ID || a.Run.HeadSHA != run.HeadSHA:
			artifact.EvidenceError = "artifact does not identify the verified workflow run and PR commit"
		case a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() || a.CreatedAt.Before(started) || (run.Attempt > 1 && a.CreatedAt.Equal(started)) || a.UpdatedAt.Before(a.CreatedAt) || a.UpdatedAt.After(updated):
			artifact.EvidenceError = "artifact timestamps do not identify the verified attempt; earlier or ambiguous artifact excluded"
		case artifact.Expired:
			artifact.EvidenceError = "artifact expired; evidence unavailable"
		default:
			// Select by artifact ID rather than its untrusted, potentially duplicated name.
			artifact.DownloadCommand = fmt.Sprintf("gh api repos/%s/%s/actions/artifacts/%d/zip > artifact-%d.zip", target.Owner, target.Repo, a.ID, a.ID)
			c.readArtifact(ctx, target, &artifact, budget)
		}
		result.Artifacts = append(result.Artifacts, artifact)
	}
	// A rerun racing collection invalidates all excerpts from this run.
	current, err := c.ActionsRun(ctx, target, run.ID)
	if err != nil || current != run {
		result.EvidenceError = "workflow attempt changed or could not be reverified during artifact collection; evidence discarded"
		for i := range result.Artifacts {
			result.Artifacts[i].Files = []ArtifactFile{}
			result.Artifacts[i].DownloadCommand = ""
		}
	}
	return result
}

func (c *prClient) readArtifact(ctx context.Context, target PRTarget, artifact *Artifact, budget *artifactBudget) {
	if artifact.Size > maxArtifactArchive || artifact.Size > int64(budget.download) || budget.download <= 0 {
		artifact.EvidenceError = "artifact archive exceeds 8 MiB or remaining download budget"
		return
	}
	if c.artifactDownload == nil {
		artifact.EvidenceError = "artifact download client unavailable"
		return
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/artifacts/%d/zip", target.Owner, target.Repo, artifact.ID), nil)
	if err != nil {
		artifact.EvidenceError = "invalid artifact download request"
		return
	}
	resp, err := c.artifactDownload.Do(req)
	if err != nil {
		artifact.EvidenceError = "artifact download unavailable (request failed)"
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		artifact.EvidenceError = fmt.Sprintf("artifact download unavailable (HTTP %d)", resp.StatusCode)
		return
	}
	limit := min(maxArtifactArchive, budget.download)
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	budget.download -= len(data)
	if err != nil || len(data) > limit {
		artifact.EvidenceError = "artifact archive unreadable or exceeds download limit"
		return
	}
	artifact.Files, artifact.EvidenceError = artifactFiles(ctx, data, artifact.Name, budget)
}

// Archives are read in memory. No artifact is extracted or executed.
func artifactFiles(ctx context.Context, data []byte, name string, budget *artifactBudget) ([]ArtifactFile, string) {
	files := []ArtifactFile{}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return files, "invalid artifact ZIP archive"
	}
	if len(z.File) > maxArtifactEntries {
		return files, "artifact archive exceeds 200 entries"
	}
	expanded := uint64(0)
	seen := make(map[string]bool)
	for _, f := range z.File {
		clean := strings.TrimSuffix(f.Name, "/")
		if clean == "" || len(clean) > 1024 || !utf8.ValidString(clean) || path.Clean(clean) != clean || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") ||
			strings.ContainsAny(clean, "\\\x00\r\n") || strings.Contains(clean, ":") || seen[clean] || (!f.Mode().IsRegular() && !f.FileInfo().IsDir()) {
			return files, "artifact archive contains unsafe or duplicate paths"
		}
		seen[clean] = true
		if f.UncompressedSize64 > maxArtifactExpanded || expanded > maxArtifactExpanded-f.UncompressedSize64 {
			return files, "artifact archive exceeds 16 MiB expanded size"
		}
		expanded += f.UncompressedSize64
	}
	if expanded > uint64(max(0, budget.expanded)) {
		return files, "artifact expanded-size budget exhausted"
	}
	budget.expanded -= int(expanded)
	for _, f := range z.File {
		if ctx.Err() != nil {
			return files, "artifact inspection interrupted"
		}
		if f.FileInfo().IsDir() {
			continue
		}
		kind := artifactFileKind(f.Name, name)
		if kind == "" {
			continue
		}
		if budget.files <= 0 {
			return files, "artifact file inventory budget exhausted"
		}
		budget.files--
		file := ArtifactFile{Path: f.Name}
		if kind == "reference" {
			file.EvidenceError = "binary artifact not inspected; download the archive for manual inspection"
		} else if budget.text <= 0 {
			file.EvidenceError = "artifact text evidence budget exhausted"
		} else {
			r, err := f.Open()
			if err != nil {
				return files, "artifact entry unreadable"
			}
			limit := min(maxEvidenceBytes, budget.text)
			data, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
			r.Close()
			if err != nil {
				return []ArtifactFile{}, "artifact entry corrupt or unreadable"
			}
			file.Truncated = len(data) > limit
			data = data[:min(len(data), limit)]
			// A byte limit can split the final UTF-8 character.
			if file.Truncated {
				for len(data) > 0 && !utf8.Valid(data) && len(data) > limit-4 {
					data = data[:len(data)-1]
				}
			}
			if !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
				file.EvidenceError = "artifact text is not valid UTF-8 or contains binary data"
			} else {
				file.Content = string(data)
				budget.text -= len(data)
			}
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return files, "no supported text reports, screenshots or traces in artifact"
	}
	return files, ""
}

func artifactFileKind(filename, artifactName string) string {
	base := strings.ToLower(path.Base(filename))
	ext := strings.ToLower(path.Ext(base))
	if ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".webp" || (ext == ".zip" && strings.Contains(base, "trace")) {
		return "reference"
	}
	if base == "error-context.md" || ext == ".log" || ext == ".txt" {
		return "text"
	}
	if ext == ".xml" || ext == ".json" {
		hint := strings.ToLower(filename + " " + artifactName)
		for _, term := range []string{"junit", "test", "report", "result", "playwright"} {
			if strings.Contains(hint, term) {
				return "text"
			}
		}
	}
	return ""
}
