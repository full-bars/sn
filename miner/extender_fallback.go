package miner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// The sibling file is independent of the state directory and the database.
// Each entry represents one launch and its number of activation posts.
type extenderFallbackLaunch struct {
	Unix  int64 `json:"unix"`
	Posts int   `json:"posts"`
}

type extenderFallbackRecord struct {
	Launches []extenderFallbackLaunch `json:"launches"`
}

func extenderFallbackPath() (string, error) {
	dir, err := providerStateDir()
	if err != nil {
		return "", err
	}
	return dir + ".extender-activation-fallback.json", nil
}

// allowExtenderFallback never refuses on an unreadable or unwritable fallback.
// A bounded cross-process lock prevents two healthy fallback writers from
// spending the same remaining allowance; failure to lock loses the count.
func allowExtenderFallback(now time.Time, posts int) bool {
	allowed, err := recordExtenderFallback(now, posts)
	if err != nil {
		tlog("[extender] extender role on: activation budget record unreadable; fallback count lost: %v\n", err)
		return true
	}
	if !allowed {
		tlog("[extender] extender role off: fallback activation budget exhausted\n")
		return false
	}
	tlog("[extender] extender role on: activation budget record unreadable; launch counted against fallback\n")
	return true
}

func recordExtenderFallback(now time.Time, posts int) (bool, error) {
	path, err := extenderFallbackPath()
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, err
	}
	lock := flock.New(path + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), probeOpenTimeout)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return false, err
	}
	if !locked {
		return false, errors.New("fallback lock unavailable")
	}
	defer lock.Unlock()

	record := extenderFallbackRecord{Launches: []extenderFallbackLaunch{}}
	f, err := os.Open(path)
	if err == nil {
		info, statErr := f.Stat()
		if statErr != nil {
			_ = f.Close()
			return false, statErr
		}
		if info.IsDir() {
			// Let the atomic rename report the unwritable destination.
			_ = f.Close()
		} else {
			data, readErr := io.ReadAll(io.LimitReader(f, 65537))
			closeErr := f.Close()
			if readErr != nil {
				return false, readErr
			}
			if closeErr != nil {
				return false, closeErr
			}
			if len(data) > 65536 {
				return false, errors.New("fallback record too large")
			}
			if err := json.Unmarshal(data, &record); err != nil || record.Launches == nil {
				return false, fmt.Errorf("invalid fallback record")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	kept := make([]extenderFallbackLaunch, 0, len(record.Launches)+1)
	used := 0
	cutoff := now.Add(-extenderActivationWindow).Unix()
	for _, launch := range record.Launches {
		if launch.Unix <= 0 || launch.Posts <= 0 || launch.Posts > extenderActivationBudget {
			return false, errors.New("invalid fallback launch")
		}
		if launch.Unix > cutoff {
			kept = append(kept, launch)
			used += launch.Posts
		}
	}
	if used+posts > extenderActivationBudget {
		return false, nil
	}
	kept = append(kept, extenderFallbackLaunch{Unix: now.Unix(), Posts: posts})
	data, err := json.Marshal(extenderFallbackRecord{Launches: kept})
	if err != nil {
		return false, err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".extender-fallback-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(append(data, '\n')); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return false, err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return true, nil
}
