package tlsfiles

import (
	"io"
	"os"
	"sync"
	"time"
)

// Cache keeps parsed certificates and CRLs until the size or modification
// time of their file changes, so that large CRLs are not parsed on every
// scrape. Failed reads are not cached. It is safe for concurrent use.
type Cache struct {
	mu    sync.Mutex
	certs map[string]entry[[]Certificate]
	crls  map[string]entry[*CRL]
}

type entry[T any] struct {
	size  int64
	mtime time.Time
	value T
}

// NewCache creates an empty cache.
func NewCache() *Cache {
	return &Cache{
		certs: map[string]entry[[]Certificate]{},
		crls:  map[string]entry[*CRL]{},
	}
}

// Certificates is LoadCertificates with caching.
func (c *Cache) Certificates(path string) ([]Certificate, error) {
	return load(&c.mu, c.certs, path, parseCertificates)
}

// CRL is LoadCRL with caching.
func (c *Cache) CRL(path string) (*CRL, error) {
	return load(&c.mu, c.crls, path, parseCRL)
}

func load[T any](mu *sync.Mutex, m map[string]entry[T], path string, parse func(string, []byte) (T, error)) (T, error) {
	var zero T
	// Stat and read the same file descriptor, so that a file replaced in
	// between cannot be cached under the metadata of its predecessor.
	f, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return zero, err
	}

	mu.Lock()
	e, ok := m[path]
	mu.Unlock()
	if ok && e.size == fi.Size() && e.mtime.Equal(fi.ModTime()) {
		return e.value, nil
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return zero, err
	}
	v, err := parse(path, data)
	if err != nil {
		mu.Lock()
		delete(m, path)
		mu.Unlock()
		return zero, err
	}
	mu.Lock()
	m[path] = entry[T]{size: fi.Size(), mtime: fi.ModTime(), value: v}
	mu.Unlock()
	return v, nil
}
