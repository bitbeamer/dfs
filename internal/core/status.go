package core

import (
	"context"
	"strings"
	"time"
)

func (s *Service) History(ctx context.Context, path string) ([]Revision, error) {
	cleaned, err := cleanPath(path)
	if err != nil {
		return nil, err
	}
	output, err := s.repo.History(ctx, cleaned)
	if err != nil {
		return nil, classify("history", cleaned, err)
	}
	var revisions []Revision
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.SplitN(line, " ", 5)
		if len(fields) < 5 {
			continue
		}
		at, _ := time.Parse("2006-01-02 15:04:05 -0700", strings.Join(fields[1:4], " "))
		revisions = append(revisions, Revision{ID: fields[0], Time: at, Summary: fields[4]})
	}
	return revisions, nil
}

func (s *Service) Health(ctx context.Context) (HealthSnapshot, error) {
	stats, err := s.repo.HealthStats(ctx)
	if err != nil {
		return HealthSnapshot{}, classify("health", "", err)
	}
	result := HealthSnapshot{
		LogicalFiles: stats.LogicalFiles, LogicalBytes: stats.LogicalBytes,
		ContentFiles: stats.ContentFiles, ContentBytes: stats.ContentBytes,
		CacheBytes: stats.CacheBytes, CacheLimitBytes: stats.CacheLimitBytes,
		MissingPinnedFiles: stats.MissingPinnedFiles, DiskAvailableBytes: stats.DiskAvailableBytes,
		Pins: make([]PinHealth, 0, len(stats.Pinned)),
	}
	for _, pin := range stats.Pinned {
		kind := KindDirectory
		switch pin.Kind {
		case "file":
			kind = KindFile
		case "missing":
			kind = KindUnknown
		}
		result.Pins = append(result.Pins, PinHealth{Pin: Pin{Path: pin.Path, Scope: PinScope(pin.Scope)}, Kind: kind,
			Status: pin.Status, LogicalFiles: pin.LogicalFiles, LogicalBytes: pin.LogicalBytes,
			MissingFiles: pin.MissingFiles, MissingBytes: pin.MissingBytes})
	}
	return result, nil
}
