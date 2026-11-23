package task

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"hwcloud/internal/config"
	"hwcloud/internal/huawei"
)

// StarGrapeRepo 完成「Star开源项目Grape代码仓」（一次性 +50）。
//
// 2026-11-23 新任务。判定链路的真实情况：
//   - 任务卡在实践中心首页（hands-on-agent），完成状态由该页面渲染；
//   - GitHub 绑定是必要条件（页面有「GitHub 账号」OAuth 绑定面板）；
//   - 成长中心 /user/tasks 的 is_finish 依赖激励服务异步结算，会滞后。
//
// 所以这里的顺序是：
//  1. 先读实践中心首页的任务卡状态（最及时）；
//  2. 已完成 → 直接返回 ok（不再访问 GitHub、不重复 Star）；
//  3. 未完成 → 读 GitHub 绑定状态；未绑定则明确返回 need_bind；
//  4. 已绑定但未完成 → 如实返回需要人工在 GitHub 点 Star，
//     并给出仓库地址（本项目不保存 GitHub 登录态，不伪造成功）。
const defaultGrapeRepo = "https://github.com/grape-dev/grape"

// StarGrapeRepo 检查/尝试完成 Grape Star 任务。
func StarGrapeRepo(headless bool) (map[string]any, error) {
	repo := defaultGrapeRepo
	if raw, err := os.ReadFile(filepath.Join(config.DataDir(), "grape_repo.txt")); err == nil {
		if v := strings.TrimSpace(string(raw)); v != "" {
			repo = v
		}
	}
	owner, name, err := parseGitHubRepo(repo)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"repo": owner + "/" + name, "url": repo, "ok": false}

	// 1. 实践中心页面状态（最及时）
	if st, err := HohTaskStatus(); err == nil {
		out["hoh_status"] = st
		if st["taskGrapeStar"] {
			out["ok"] = true
			out["msg"] = "实践中心已显示 Grape Star 已完成"
			return out, nil
		}
	}

	// 2. GitHub 是否已绑定（实践中心绑定面板）
	bound := false
	if session, serr := huawei.New(""); serr == nil {
		if b, berr := hohBindingStatus(session); berr == nil {
			bound = b.HasGitHub
			out["github_bound"] = bound
			out["github_user"] = b.GitHubLogin
		}
	}
	if !bound {
		out["need_bind"] = true
		out["msg"] = "需要先在实践中心绑定 GitHub 账号，再点 Star"
		return out, nil
	}

	// 3. 已绑定但页面仍未完成：如实报告需要人工 Star（本项目无 GitHub 登录态）
	out["need_manual"] = true
	out["msg"] = "GitHub 已绑定，但实践中心尚未显示完成；需要在 GitHub 对 " + owner + "/" + name + " 点 Star"
	return out, nil
}

// parseGitHubRepo 从 URL 或 owner/repo 解析出两段。
func parseGitHubRepo(s string) (string, string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".git")
	re := regexp.MustCompile(`(?:github\.com[/:])([^/]+)/([^/?#]+)`)
	if m := re.FindStringSubmatch(s); len(m) == 3 {
		return m[1], m[2], nil
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1], nil
	}
	return "", "", fmt.Errorf("无法解析 GitHub 仓库地址: %s", s)
}

// hohBinding 是实践中心的账号绑定状态。
type hohBinding struct {
	HasGitHub   bool
	GitHubLogin string
}

// hohBindingStatus 读实践中心绑定状态（只读）。
func hohBindingStatus(session *huawei.Session) (hohBinding, error) {
	var out hohBinding
	u, err := hohLogin(session)
	if err != nil {
		return out, err
	}
	body, err := hohRequest(session, http.MethodGet, "/api/binding/status?hw_account_name="+u.Username, nil)
	if err != nil {
		return out, err
	}
	var resp struct {
		Bindings []struct {
			Provider string `json:"provider"`
			Login    string `json:"login"`
			Status   string `json:"status"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return out, err
	}
	for _, b := range resp.Bindings {
		if b.Provider == "github" && b.Status == "bound" {
			out.HasGitHub = true
			out.GitHubLogin = b.Login
		}
	}
	return out, nil
}

var _ = time.Now
