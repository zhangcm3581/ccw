package sync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// 云端副本管理（2026-08-01）。
//
// 每个本地目录在云端各有一份副本（工作区）。换过机器、试过几个目录、
// 项目改过名之后，云端会攒下一堆再也用不到的副本，把项目的磁盘配额吃光。
// 这里让客户端能看见它们的大小并删掉。
//
// **删除是硬删除，不是墓碑。**普通的 delete 写 tombstone 是为了把"这个文件
// 被删了"同步给其他设备；而删云端副本要的恰恰相反——本地文件必须原样保留，
// 下次连上去重新上传。写墓碑会把删除传播出去，把用户其他机器上的文件也删掉。

// WorkspaceInfo是一个云端副本的用量概览。
type WorkspaceInfo struct {
	WS    string `json:"ws"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
}

// PurgeStore是硬删除需要的存储能力。与RevisionStore分开：
// 它是一个破坏性操作，不该混进每条同步连接都要实现的那个接口里。
type PurgeStore interface {
	PurgeWorkspace(ctx context.Context, projectID, ws string) (int64, error)
}

// ListWorkspaces按工作区前缀汇总项目下的全部副本。
//
// 直接用 Manifest 分组，不新增存储方法：机队上限是 3 台×3 项目，
// 一个项目的索引行数以万计，分组的开销远小于再维护一张汇总表的复杂度。
// 墓碑不计入大小——它们不占盘。
func ListWorkspaces(ctx context.Context, st RevisionStore, projectID string) ([]WorkspaceInfo, error) {
	entries, err := st.Manifest(ctx, projectID)
	if err != nil {
		return nil, err
	}
	agg := map[string]*WorkspaceInfo{}
	for _, e := range entries {
		ws, _, ok := splitKey(e.Path)
		if !ok {
			continue // 没有工作区前缀的旧数据：不归任何副本，也不该被误删
		}
		w := agg[ws]
		if w == nil {
			w = &WorkspaceInfo{WS: ws}
			agg[ws] = w
		}
		if e.Deleted {
			continue
		}
		w.Bytes += e.Size
		w.Files++
	}
	out := make([]WorkspaceInfo, 0, len(agg))
	for _, w := range agg {
		out = append(out, *w)
	}
	// 大的排前面：要腾空间时最该先看的就是它们。
	sortWorkspaces(out)
	return out, nil
}

// splitKey把索引里的 "<ws>/rel" 拆开。
func splitKey(key string) (ws, rel string, ok bool) {
	i := strings.IndexByte(key, '/')
	if i <= 0 || i == len(key)-1 {
		return "", "", false
	}
	ws = key[:i]
	if !ValidWorkspace(ws) {
		return "", "", false
	}
	return ws, key[i+1:], true
}

func sortWorkspaces(ws []WorkspaceInfo) {
	for i := 1; i < len(ws); i++ {
		for j := i; j > 0 && (ws[j].Bytes > ws[j-1].Bytes ||
			(ws[j].Bytes == ws[j-1].Bytes && ws[j].WS < ws[j-1].WS)); j-- {
			ws[j], ws[j-1] = ws[j-1], ws[j]
		}
	}
}

// PurgeWorkspace删掉一个云端副本：索引行与磁盘文件都清掉。
//
// **ws 必须先过 ValidWorkspace**——它会被拼进文件系统路径，
// 放行 ".." 就是任意目录删除。这是本函数最要紧的一行。
func PurgeWorkspace(ctx context.Context, st PurgeStore, projectID, root, ws string) (int64, error) {
	if !ValidWorkspace(ws) {
		return 0, ErrUnsafePath
	}
	freed, err := st.PurgeWorkspace(ctx, projectID, ws)
	if err != nil {
		return 0, err
	}
	if root != "" {
		// 索引已经清了，磁盘删不掉也不该报失败：留下的是不再被引用的文件，
		// 下一次 reconcileCloud 会把它们当成"容器里新建的文件"重新收进来。
		// 那比"索引清了、客户端以为删成功了、其实报了个错"要好排查。
		_ = os.RemoveAll(filepath.Join(root, ws))
	}
	return freed, nil
}
