// Package certs serves a TLS certificate from files and picks up renewed files without a restart.
package certs

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"time"
)

// Reloader re-reads the certificate when the files change, checking at most once per interval.
type Reloader struct {
	certFile, keyFile string
	interval          time.Duration

	mu        sync.Mutex
	cert      *tls.Certificate
	modTime   time.Time
	lastCheck time.Time
}

// New loads the certificate once; it fails if the files are missing or do not match.
func New(certFile, keyFile string, interval time.Duration) (*Reloader, error) {
	r := &Reloader{certFile: certFile, keyFile: keyFile, interval: interval}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reloader) load() error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load TLS certificate: %w", err)
	}
	mod, err := r.newestModTime()
	if err != nil {
		return err
	}
	r.cert, r.modTime = &cert, mod
	return nil
}

func (r *Reloader) newestModTime() (time.Time, error) {
	var newest time.Time
	for _, f := range []string{r.certFile, r.keyFile} {
		st, err := os.Stat(f) // follows symlinks, as in /etc/letsencrypt/live
		if err != nil {
			return time.Time{}, err
		}
		if st.ModTime().After(newest) {
			newest = st.ModTime()
		}
	}
	return newest, nil
}

// GetCertificate is used as tls.Config.GetCertificate. If a renewed pair cannot be loaded
// (for example the key is written after the certificate), the previous one keeps being served.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.lastCheck) >= r.interval {
		r.lastCheck = time.Now()
		if mod, err := r.newestModTime(); err == nil && !mod.Equal(r.modTime) {
			old, oldMod := r.cert, r.modTime
			if err := r.load(); err != nil {
				r.cert, r.modTime = old, oldMod
			}
		}
	}
	return r.cert, nil
}
