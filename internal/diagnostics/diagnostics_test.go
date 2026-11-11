package diagnostics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentEvidenceAllowsOnlyFixedKeysAndNumericCodes(t *testing.T) {
	c := &Collector{}
	var jobs sync.WaitGroup
	for range 12 {
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			for range 50 {
				c.Record("callback", "received")
				c.Record("payload-secret", "received")
				c.Record("callback", "payload-secret")
				c.API(999, 403)
				c.Snapshot()
			}
		}()
	}
	jobs.Wait()
	s := c.Snapshot()
	data, _ := json.Marshal(s)
	if s.Counts["callback"]["received"] != 600 || len(s.Counts) != 1 || len(s.Counts["callback"]) != 1 || strings.Contains(string(data), "secret") || s.LastAPICode != 999 || s.LastHTTPStatus != 403 {
		t.Fatal("unsafe or inconsistent counters")
	}
	s.Counts["callback"]["received"] = 0
	if c.Snapshot().Counts["callback"]["received"] != 600 {
		t.Fatal("snapshot aliases live counters")
	}
}

func TestReceiptExclusiveAtomicAndPrivate(t *testing.T) {
	c := &Collector{}
	dir := t.TempDir()
	path := filepath.Join(dir, "interaction.json")
	f, err := c.CreateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateFile(path); err == nil {
		t.Fatal("existing receipt overwritten")
	}
	other := &Collector{}
	other.Record("draft_send", "queued")
	if err := f.SaveFrom(other); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	var s Snapshot
	if err != nil || json.Unmarshal(data, &s) != nil || s.Counts["draft_send"]["queued"] != 1 {
		t.Fatal("snapshot not published")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("receipt permissions too broad")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temporary snapshot leaked")
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateFile(link); err == nil {
		t.Fatal("followed a preexisting symlink")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(data) {
		t.Fatal("failed create modified original receipt")
	}
}

func TestApprovalSettingsConcurrentSnapshotsAreAtomicAndPayloadFree(t *testing.T) {
	c := &Collector{}
	var jobs sync.WaitGroup
	for range 8 {
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			for range 100 {
				c.ApprovalSettings("user", "on-request")
				c.ApprovalSettings("auto_review", "never")
				c.ApprovalSettings("payload-secret", "payload-secret")
				s := c.Snapshot()
				var reviewers, policies uint64
				for _, count := range s.Counts["approval_reviewer"] {
					reviewers += count
				}
				for _, count := range s.Counts["approval_policy"] {
					policies += count
				}
				if reviewers != policies {
					t.Error("snapshot split an approval settings observation")
				}
				switch s.LastApprovalReviewer {
				case "user":
					if s.LastApprovalPolicy != "on-request" {
						t.Error("snapshot mixed settings pairs")
					}
				case "auto_review":
					if s.LastApprovalPolicy != "never" {
						t.Error("snapshot mixed settings pairs")
					}
				case "unknown":
					if s.LastApprovalPolicy != "unknown" {
						t.Error("unknown value was retained")
					}
				default:
					t.Error("unrecognized reviewer retained")
				}
			}
		}()
	}
	jobs.Wait()
	s := c.Snapshot()
	data, _ := json.Marshal(s)
	if s.Counts["approval_reviewer"]["unknown"] != 800 || s.Counts["approval_policy"]["unknown"] != 800 || strings.Contains(string(data), "payload-secret") {
		t.Fatal("unsafe concurrent approval evidence")
	}
}
