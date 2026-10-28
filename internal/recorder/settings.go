package recorder

import (
	"encoding/binary"
	"errors"

	bolt "go.etcd.io/bbolt"
)

type UISettings struct {
	RefreshIntervalSeconds int  `json:"refresh_interval_seconds"`
	IgnoreRejected         bool `json:"ignore_rejected"`
}

type settingsUpdate struct {
	Interval       *int  `json:"refresh_interval_seconds"`
	IgnoreRejected *bool `json:"ignore_rejected"`
}

func (p settingsUpdate) validate() error {
	if p.Interval == nil && p.IgnoreRejected == nil {
		return errors.New("provide refresh_interval_seconds or ignore_rejected")
	}
	if p.Interval != nil && (*p.Interval < 1 || *p.Interval > 3600) {
		return errors.New("refresh_interval_seconds must be 1..3600")
	}
	return nil
}

func (s *Store) Settings(fallbackInterval int) UISettings {
	settings := UISettings{RefreshIntervalSeconds: fallbackInterval}
	_ = s.view(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(storageMeta)
		if raw := bucket.Get([]byte("refresh_interval")); len(raw) == 8 {
			settings.RefreshIntervalSeconds = int(binary.BigEndian.Uint64(raw))
		}
		settings.IgnoreRejected = string(bucket.Get([]byte("ignore_rejected"))) == "1"
		return nil
	})
	return settings
}

func (s *Store) SetSettings(p settingsUpdate) error {
	if err := p.validate(); err != nil {
		return err
	}
	// Capture values before the transaction so compaction can replay them unchanged.
	var interval []byte
	if p.Interval != nil {
		interval = make([]byte, 8)
		binary.BigEndian.PutUint64(interval, uint64(*p.Interval))
	}
	var ignoreRejected []byte
	if p.IgnoreRejected != nil {
		ignoreRejected = []byte("0")
		if *p.IgnoreRejected {
			ignoreRejected = []byte("1")
		}
	}
	return s.update(4096, func(tx *bolt.Tx) error {
		bucket := tx.Bucket(storageMeta)
		if interval != nil {
			if err := bucket.Put([]byte("refresh_interval"), interval); err != nil {
				return err
			}
		}
		if ignoreRejected != nil {
			return bucket.Put([]byte("ignore_rejected"), ignoreRejected)
		}
		return nil
	})
}
