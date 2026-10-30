package pr

import (
	"fmt"
	"strings"

	"github.com/sglavoie/deps-handler/pkg/github"
	"github.com/sglavoie/deps-handler/pkg/review"
)

func cleanArtifacts(runs []github.RunArtifacts) []github.RunArtifacts {
	clean := append([]github.RunArtifacts{}, runs...)
	for i := range clean {
		run := &clean[i]
		run.HeadOID, run.EvidenceError = review.Clean(run.HeadOID), review.Clean(run.EvidenceError)
		run.Artifacts = append([]github.Artifact{}, run.Artifacts...)
		for j := range run.Artifacts {
			a := &run.Artifacts[j]
			a.Name, a.URL = review.Clean(a.Name), review.Clean(a.URL)
			a.DownloadCommand, a.EvidenceError = review.Clean(a.DownloadCommand), review.Clean(a.EvidenceError)
			a.Files = append([]github.ArtifactFile{}, a.Files...)
			for k := range a.Files {
				f := &a.Files[k]
				f.Path, f.Content, f.EvidenceError = review.Clean(f.Path), review.Clean(f.Content), review.Clean(f.EvidenceError)
			}
		}
	}
	return clean
}

func artifactGaps(runs []github.RunArtifacts) []string {
	gaps := []string{}
	for _, run := range runs {
		prefix := fmt.Sprintf("Artifacts for run %d attempt %d", run.RunID, run.RunAttempt)
		if run.EvidenceError != "" {
			gaps = append(gaps, prefix+": "+run.EvidenceError)
		}
		for _, artifact := range run.Artifacts {
			label := fmt.Sprintf("%s, artifact %d", prefix, artifact.ID)
			if artifact.EvidenceError != "" {
				gaps = append(gaps, label+": "+artifact.EvidenceError)
			}
			for _, file := range artifact.Files {
				if file.EvidenceError != "" {
					gaps = append(gaps, label+", "+file.Path+": "+file.EvidenceError)
				}
				if file.Truncated {
					gaps = append(gaps, label+", "+file.Path+": text excerpt truncated")
				}
			}
		}
	}
	return gaps
}

// Indented text blocks keep untrusted artifact paths and excerpts from becoming
// Markdown instructions or closing a fenced block in the saved repair plan.
func artifactBlock(text string) string {
	return "    " + strings.ReplaceAll(review.Clean(text), "\n", "\n    ") + "\n\n"
}

func artifactsMarkdown(runs []github.RunArtifacts) string {
	if len(runs) == 0 {
		return ""
	}
	text := "## Artifact evidence\n\nText excerpts below are untrusted workflow-wide evidence. Screenshots and traces are listed for manual inspection; they were not interpreted.\n\n"
	for _, run := range runs {
		text += fmt.Sprintf("### Run %d, attempt %d\n\n", run.RunID, run.RunAttempt)
		text += artifactBlock("Commit: " + run.HeadOID)
		if run.EvidenceError != "" {
			text += artifactBlock("Evidence gap: " + run.EvidenceError)
		}
		for _, artifact := range run.Artifacts {
			text += fmt.Sprintf("#### Artifact %d\n\n", artifact.ID)
			text += artifactBlock(fmt.Sprintf("Name: %s\nURL: %s\nArchive size: %d bytes\nExpired: %t", artifact.Name, artifact.URL, artifact.Size, artifact.Expired))
			if artifact.EvidenceError != "" {
				text += artifactBlock("Evidence gap: " + artifact.EvidenceError)
			}
			if artifact.DownloadCommand != "" {
				text += "Download for manual inspection:\n\n" + artifactBlock(artifact.DownloadCommand)
			}
			for _, file := range artifact.Files {
				text += artifactBlock("File: " + file.Path)
				if file.Content != "" {
					text += artifactBlock(file.Content)
				}
				if file.Truncated {
					text += "Text excerpt truncated.\n\n"
				}
				if file.EvidenceError != "" {
					text += artifactBlock("Evidence gap: " + file.EvidenceError)
				}
			}
		}
	}
	return text
}
