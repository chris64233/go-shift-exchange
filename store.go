package goshiftexchange

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// Store 是事件追加存储。Append 必须保证：事件以单调递增的 Seq 落盘，
// 一旦成功返回，后续 Load 一定能读到该事件。
type Store interface {
	// Append 以原子方式追加一批事件（一批要么全部可见，要么全部不可见）。
	Append(events []Event) error
	// Load 按 Seq 顺序重放全部事件。
	Load(fn func(Event) error) error
}

// MemoryStore 仅保存在内存中的事件存储，用于测试。
type MemoryStore struct {
	mu     sync.Mutex
	events []Event
}

// NewMemoryStore 创建内存事件存储。
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

// Append 实现 Store。
func (s *MemoryStore) Append(events []Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
	return nil
}

// Load 实现 Store。
func (s *MemoryStore) Load(fn func(Event) error) error {
	s.mu.Lock()
	events := append([]Event(nil), s.events...)
	s.mu.Unlock()
	for _, ev := range events {
		if err := fn(ev); err != nil {
			return err
		}
	}
	return nil
}

// FileStore 把事件以 JSON Lines 形式追加持久化到单个文件：
// 每行一个 JSON 事件，写入后 fsync，崩溃也不会出现半行。
type FileStore struct {
	mu sync.Mutex
	f  *os.File
}

// NewFileStore 打开（必要时创建）事件日志文件。返回前不做重放，
// 重放由 NewService 在构造时通过 Load 完成。
func NewFileStore(path string) (*FileStore, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("goshiftexchange: open event log: %w", err)
	}
	return &FileStore{f: f}, nil
}

// Close 关闭底层文件。
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

// Append 实现 Store：逐行编码，整批写完后统一 fsync。
func (s *FileStore) Append(events []Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	bw := bufio.NewWriter(s.f)
	enc := json.NewEncoder(bw)
	for i := range events {
		if err := enc.Encode(&events[i]); err != nil {
			return fmt.Errorf("goshiftexchange: encode event: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("goshiftexchange: flush event log: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("goshiftexchange: fsync event log: %w", err)
	}
	return nil
}

// Load 实现 Store：从头顺序读取完整行。载荷保留为 map[string]any，
// 由 Service.apply 按事件类型反序列化为具体结构（内存/文件两条路径共用）。
func (s *FileStore) Load(fn func(Event) error) error {
	s.mu.Lock()
	path := s.f.Name()
	s.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("goshiftexchange: open event log for read: %w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(bufio.NewReader(f))
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("goshiftexchange: decode event: %w", err)
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
}
