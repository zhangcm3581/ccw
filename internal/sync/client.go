package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LocalIndex：项目根.cclaude/index.json，保存三方判断所需基线。
type LocalIndex struct{ Root string }

func (l LocalIndex) path() string { return filepath.Join(l.Root, ".cclaude", "index.json") }

func (l LocalIndex) Load() ([]LocalEntry, error) {
	b, err := os.ReadFile(l.path())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []LocalEntry
	return out, json.Unmarshal(b, &out)
}

func (l LocalIndex) Save(es []LocalEntry) error {
	if err := os.MkdirAll(filepath.Dir(l.path()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(es, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(l.path(), b, 0o644)
}

// ScanDir扫描当前目录状态（与DirStore.Manifest同排除规则）；
// 结果经BuildLocal与LocalIndex基线合成后才能进Diff。
func ScanDir(root string) ([]FileEntry, error) {
	var out []FileEntry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, root+string(os.PathSeparator)))
		if d.IsDir() {
			if rel != "" && DefaultExcluded(rel+"/") {
				return filepath.SkipDir
			}
			return nil
		}
		// 只扫普通文件（与DirStore.Manifest一致）：符号链接不同步——
		// 云端跟随符号链接等于把root之外的内容纳入清单，是逃逸面的一部分。
		if !d.Type().IsRegular() {
			return nil
		}
		if DefaultExcluded(rel) || strings.HasPrefix(filepath.Base(p), ".cclaude.tmp.") {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		out = append(out, FileEntry{Path: rel, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	return out, err
}
