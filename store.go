package goshiftexchange

import (
	"bufio"
	"bytes"
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
	s := &FileStore{f: f}
	if err := s.recoverTornTail(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return s, nil
}

// recoverTornTail 截断文件末尾不带换行符的不完整行（追加到一半掉电留下的半行）。
// 以换行结尾的完整事件一律保留；整个文件都没有换行时视为全是半行，清空重来。
func (s *FileStore) recoverTornTail() error {
	fi, err := s.f.Stat()
	if err != nil {
		return fmt.Errorf("goshiftexchange: stat event log: %w", err)
	}
	size := fi.Size()
	if size == 0 {
		return nil
	}
	// 半行必在文件末尾。从后往前分块扫描最后一个换行：JSON 事件单行编码，
	// 换行只会出现在两行之间，因此最后一个换行之后的全部内容就是半行。
	const chunk = 64 * 1024
	var cut int64 = -1
	for end := size; end > 0; {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		buf := make([]byte, end-start)
		n, err := s.f.ReadAt(buf, start)
		if err != nil && err != io.EOF {
			return fmt.Errorf("goshiftexchange: read event log tail: %w", err)
		}
		buf = buf[:n]
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			cut = start + int64(i) + 1
			break
		}
		end = start
	}
	if cut == size {
		return nil // 最后一个换行恰好在末尾：末行完整，无需恢复。
	}
	if cut < 0 {
		cut = 0 // 整个文件都没有换行：全部视为半行。
	}
	if err := s.f.Truncate(cut); err != nil {
		return fmt.Errorf("goshiftexchange: truncate torn event tail: %w", err)
	}
	if _, err := s.f.Seek(cut, io.SeekStart); err != nil {
		return fmt.Errorf("goshiftexchange: seek event log: %w", err)
	}
	return nil
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
	// 旧版本文件或手工编辑后可能缺少末尾换行，先补齐，避免新事件粘在上一行后。
	fi, err := s.f.Stat()
	if err != nil {
		return fmt.Errorf("goshiftexchange: stat event log: %w", err)
	}
	if fi.Size() > 0 {
		tail := make([]byte, 1)
		if _, err := s.f.ReadAt(tail, fi.Size()-1); err != nil {
			return fmt.Errorf("goshiftexchange: read event log tail: %w", err)
		}
		if tail[0] != '\n' {
			if _, err := bw.WriteString("\n"); err != nil {
				return fmt.Errorf("goshiftexchange: separate event line: %w", err)
			}
		}
	}
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
