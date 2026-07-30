package scheduler

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/nauski/hoard/internal/restic"
	"github.com/nauski/hoard/internal/state"
)

// verifyMaxFileBytes caps the size of a sampled file so a fire-drill stays cheap.
const verifyMaxFileBytes = 64 << 20

// Verify runs the restore fire-drill: sample a file from a random client's
// latest snapshot, restore it with content verification, and record the result.
// A successful restore validates blob hashes end-to-end, so it proves the data
// comes back — stronger than `restic check` alone.
func (s *Scheduler) Verify(ctx context.Context) {
	if !s.acquire("verify") {
		return
	}
	defer s.release()

	start := time.Now()
	rec := func(v state.VerifyResult, jobMsg string, jobOK bool) {
		s.store.SetVerify(v)
		s.store.RecordJob(state.JobResult{
			Job: "verify", StartedAt: start, EndedAt: time.Now(), OK: jobOK, Message: jobMsg,
		})
	}

	// Pick a random client that has a snapshot.
	clients := s.store.Snapshot().Clients
	var hosts []state.Client
	for _, c := range clients {
		if c.SnapshotID != "" {
			hosts = append(hosts, c)
		}
	}
	if len(hosts) == 0 {
		rec(state.VerifyResult{Time: time.Now(), OK: true, Err: ""}, "no clients to verify (skipped)", true)
		return
	}
	client := hosts[rand.Intn(len(hosts))]

	hot := s.hotC()
	files, err := hot.ListFiles(ctx, client.SnapshotID)
	if err != nil {
		s.failVerify(rec, client.Hostname, "", "list files: "+err.Error())
		return
	}
	var eligible []restic.LsEntry
	for _, f := range files {
		if f.Size > 0 && f.Size <= verifyMaxFileBytes {
			eligible = append(eligible, f)
		}
	}
	if len(eligible) == 0 {
		rec(state.VerifyResult{Time: time.Now(), OK: true, Client: client.Hostname}, "no eligible file to sample (skipped)", true)
		return
	}
	pick := eligible[rand.Intn(len(eligible))]

	tmp, err := os.MkdirTemp("", "hoard-verify-")
	if err != nil {
		s.failVerify(rec, client.Hostname, pick.Path, "tempdir: "+err.Error())
		return
	}
	defer os.RemoveAll(tmp)

	if _, _, err := hot.Restore(ctx, client.SnapshotID, pick.Path, tmp, "always", true, restic.RestoreHooks{}); err != nil {
		s.failVerify(rec, client.Hostname, pick.Path, "restore: "+err.Error())
		return
	}

	// Confirm the restored file exists with the expected size.
	got, serr := restoredSize(tmp)
	if serr != nil || got != pick.Size {
		s.failVerify(rec, client.Hostname, pick.Path,
			fmt.Sprintf("size mismatch: want %d got %d (%v)", pick.Size, got, serr))
		return
	}

	rec(state.VerifyResult{Time: time.Now(), OK: true, Client: client.Hostname, File: pick.Path, Bytes: pick.Size},
		"verified "+client.Hostname+":"+pick.Path, true)
	s.log.Info("verify ok", "client", client.Hostname, "file", pick.Path)
}

func (s *Scheduler) failVerify(rec func(state.VerifyResult, string, bool), host, file, msg string) {
	rec(state.VerifyResult{Time: time.Now(), OK: false, Client: host, File: file, Err: msg}, "verify FAILED: "+msg, false)
	s.log.Error("verify failed", "err", msg)
	s.alert("restore verification FAILED", host+": "+msg)
}

// restoredSize returns the size of the single largest regular file under dir
// (the sampled file restic re-created under target, preserving its path).
func restoredSize(dir string) (uint64, error) {
	var size uint64
	var found bool
	err := filepathWalkSize(dir, &size, &found)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("no file restored")
	}
	return size, nil
}

func filepathWalkSize(dir string, size *uint64, found *bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		full := dir + string(os.PathSeparator) + e.Name()
		if e.IsDir() {
			if err := filepathWalkSize(full, size, found); err != nil {
				return err
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if uint64(info.Size()) > *size {
			*size = uint64(info.Size())
		}
		*found = true
	}
	return nil
}
