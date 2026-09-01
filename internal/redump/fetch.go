package redump

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DreamcastURL serves the datfile as a zip.
	DreamcastURL = "http://redump.org/datfile/dc/"

	requestTimeout = 2 * time.Minute

	// Bounds a server gone wrong; the real datfile is under a megabyte.
	maxDATBytes = 64 << 20

	dirMode = 0o750

	cachePrefix = "redump-dc-"
	cacheSuffix = ".dat"
)

// CacheDir is where Fetch keeps datfiles between runs.
func CacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("cache dir: %w", err)
	}

	return filepath.Join(dir, "swirl"), nil
}

// Fetch returns a datfile, downloading only when redump serves a build the cache lacks.
// Plain HTTP with no signature: a match cannot prove the datfile authentic.
func Fetch(ctx context.Context, agent string, notice io.Writer) (string, error) {
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}

	return fetchTo(ctx, agent, DreamcastURL, dir, notice)
}

func fetchTo(ctx context.Context, agent, url, dir string, notice io.Writer) (string, error) {
	// No ETag, and If-Modified-Since is ignored: freshness rides on the Content-Disposition build stamp.
	build, headErr := currentBuild(ctx, agent, url)
	if headErr == nil {
		path := filepath.Join(dir, cachePrefix+build+cacheSuffix)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	if headErr != nil {
		// A datfile from a previous run still describes real discs.
		if path := newestCached(dir); path != "" {
			return path, nil
		}

		return "", headErr
	}

	if notice != nil {
		fmt.Fprintf(notice, "Fetching %s\n", url)
	}

	dat, err := download(ctx, agent, url)
	if err != nil {
		return "", err
	}

	return store(dir, build, dat)
}

// Build stamp, e.g. "Sega - Dreamcast - Datfile (1516) (2026-06-14 18-25-41).zip".
func currentBuild(ctx context.Context, agent, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", fmt.Errorf("redump: %w", err)
	}

	req.Header.Set("User-Agent", agent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("redump: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("redump: %s", resp.Status)
	}

	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err != nil {
		return "", fmt.Errorf("redump: naming the datfile: %w", err)
	}

	name := strings.TrimSuffix(params["filename"], ".zip")
	if name == "" {
		return "", errors.New("redump: the download is unnamed, so its build is unknown")
	}

	return slug(name), nil
}

func slug(name string) string {
	var b strings.Builder

	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case !strings.HasSuffix(b.String(), "-"):
			b.WriteRune('-')
		}
	}

	return strings.Trim(b.String(), "-")
}

func newestCached(dir string) string {
	entries, err := filepath.Glob(filepath.Join(dir, cachePrefix+"*"+cacheSuffix))
	if err != nil || len(entries) == 0 {
		return ""
	}

	newest, newestAt := "", time.Time{}

	for _, path := range entries {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}

		if info.ModTime().After(newestAt) {
			newest, newestAt = path, info.ModTime()
		}
	}

	return newest
}

func store(dir, build string, dat []byte) (string, error) {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return "", fmt.Errorf("cache dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, cachePrefix+"*.part")
	if err != nil {
		return "", fmt.Errorf("cache datfile: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(dat); err != nil {
		tmp.Close()

		return "", fmt.Errorf("cache datfile: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("cache datfile: %w", err)
	}

	path := filepath.Join(dir, cachePrefix+build+cacheSuffix)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("cache datfile: %w", err)
	}

	// A superseded build that will not delete is no reason to fail the download.
	stale, err := filepath.Glob(filepath.Join(dir, cachePrefix+"*"+cacheSuffix))
	if err == nil {
		for _, old := range stale {
			if old != path {
				_ = os.Remove(old)
			}
		}
	}

	return path, nil
}

func download(ctx context.Context, agent, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("redump: %w", err)
	}

	req.Header.Set("User-Agent", agent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("redump: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("redump: %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDATBytes))
	if err != nil {
		return nil, fmt.Errorf("redump: %w", err)
	}

	return unzipDAT(body)
}

func unzipDAT(body []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, fmt.Errorf("redump: %w", err)
	}

	for _, f := range zr.File {
		if !strings.EqualFold(filepath.Ext(f.Name), cacheSuffix) {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("redump: %w", err)
		}
		defer rc.Close()

		dat, err := io.ReadAll(io.LimitReader(rc, maxDATBytes))
		if err != nil {
			return nil, fmt.Errorf("redump: %w", err)
		}

		return dat, nil
	}

	return nil, errors.New("redump: the download held no datfile")
}
