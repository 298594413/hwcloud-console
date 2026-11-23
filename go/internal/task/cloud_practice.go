package task

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
	"hwcloud/internal/config"
)

// 鸿蒙实践中心的「云端实践」(+100，每周)。
//
// 完整链路（2026-11-23 实测验证，AI 编译打包成功、积分到账）：
//  1. 用华为云登录态打开 harmonybp 案例详情页
//  2. 点「云端实践」主按钮 → 新窗口 version-select?repo_url=<案例公仓>
//  3. 选「体验版」（免费、不消耗核时）→ 立即进入
//  4. 项目管理面板：点「新建项目」→ 表单自动填好（名称/目录/案例公仓地址）→ 点「创建」
//  5. 创建后点「进入开发环境」→ 切到 CodeArts Web tab
//  6. 在 chat-textarea 发口令：拉取案例公仓代码 + 编译打包
//  7. AI 编译（可能报 SDK 版本错要改文件）→ 循环点 button.perm-btn.always 授权
//  8. 检测「编译成功」→ 关机收尾
//
// 用体验版环境：免费、秒级启动、不消耗核时，关闭后数据释放。

const (
	harmonyCasesURL = "https://harmonybp.developer.huaweicloud.com/cases"
	buildPromptTpl  = "拉取 %s 代码到当前项目目录，" +
		"然后执行编译打包（用 hvigorw assembleHap 或项目对应的编译命令），确保编译成功。直接执行不要询问我。"
	lastCaseFile = "harmony_last_case.txt"
)

// RunCloudPractice 完成「云端实践」：案例详情 → 云端实践 → 体验版 → 新建项目 → CodeArts Web 发编译口令 → 授权 → 编译成功。
func RunCloudPractice(headless bool) (map[string]any, error) {
	pw, err := playwright.Run()
	if err != nil {
		return nil, err
	}
	defer pw.Stop()

	br, ctx, err := browser.NewHuaweiContext(pw, headless)
	if err != nil {
		return nil, err
	}
	defer br.Close()
	defer ctx.Close()

	page, err := ctx.NewPage()
	if err != nil {
		return nil, err
	}
	defer func() { _ = page.Close() }()

	out := map[string]any{"ok": false}

	// 1) 打开案例列表页，挑一个与上次不同的案例（平台规则：同一案例30天内重复不给分）
	if _, err := page.Goto(harmonyCasesURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(90000),
	}); err != nil {
		return nil, fmt.Errorf("打开案例列表失败: %v", err)
	}
	page.WaitForTimeout(8000)
	chosen, err := pickNextCase(page)
	if err != nil {
		return nil, fmt.Errorf("挑选案例失败: %v", err)
	}
	caseURL := harmonyCasesURL + "/" + chosen.ID
	out["case_id"] = chosen.ID
	out["case_title"] = chosen.Title
	out["case_url"] = caseURL
	logStep("[cloud-practice] 选定案例: %s (%s)", chosen.Title, chosen.ID)

	// 记录本次案例 ID（下次轮换避开）
	defer saveLastCase(chosen.ID)

	// 2) 打开案例详情页
	if _, err := page.Goto(caseURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(90000),
	}); err != nil {
		return nil, fmt.Errorf("打开案例详情失败: %v", err)
	}
	page.WaitForTimeout(8000)
	// 从详情页读真实「案例公仓」URL，覆盖占位值
	repoURL := ""
	raw, _ := page.Evaluate(`() => {
		const a = [...document.querySelectorAll('a[href]')].find(x =>
			/harmony-practice-center/.test(x.href) && /\.git$/.test(x.href));
		return a ? a.href : '';
	}`)
	repoURL, _ = raw.(string)
	if repoURL != "" {
		chosen.PublicRepo = repoURL
		logStep("[cloud-practice] 案例公仓: %s", repoURL)
	}

	// 2) 点「云端实践」主按钮 → ExpectPopup 拿新窗口
	popup, err := page.ExpectPopup(func() error {
		btn := page.Locator(`button.cp-btn--primary:has-text("云端实践")`).First()
		n, _ := btn.Count()
		if n == 0 {
			return fmt.Errorf("未找到「云端实践」主按钮")
		}
		return btn.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(10000)})
	}, playwright.PageExpectPopupOptions{Timeout: playwright.Float(30000)})
	if err != nil {
		return nil, fmt.Errorf("点云端实践失败: %v", err)
	}
	defer func() { _ = popup.Close() }()
	out["version_select_url"] = popup.URL()
	logStep("[cloud-practice] 已打开版本选择页")

	// 3) 选「体验版」→ 立即进入
	popup.WaitForTimeout(4000)
	if !clickTextExact(popup, "体验版") {
		// 兜底：任意含"体验版"的元素
		l := popup.Locator(`text=体验版`).First()
		if n, _ := l.Count(); n > 0 {
			_ = l.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(6000)})
		}
	}
	popup.WaitForTimeout(1000)
	enter := popup.Locator(`button:has-text("立即进入")`).Last()
	if n, _ := enter.Count(); n > 0 {
		_ = enter.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(10000)})
	}
	logStep("[cloud-practice] 已选体验版进入")
	popup.WaitForTimeout(25000) // 等环境 + CodeArts frame 加载

	// 4) 项目管理：新建项目 → 创建
	if err := createHarmonyProject(popup); err != nil {
		out["create_project_err"] = err.Error()
		// 若项目已存在（之前的残留），继续往下走
		logStep("[cloud-practice] 新建项目: %v（可能是已存在）", err)
	} else {
		out["project_created"] = true
		logStep("[cloud-practice] 项目已创建")
	}

	// 5) 进入开发环境
	entered := clickAnyText(popup, []string{"进入开发环境"})
	if entered {
		logStep("[cloud-practice] 已进入开发环境")
	}
	popup.WaitForTimeout(15000)

	// 6) 找到 CodeArts Web 的 chat-textarea 并发编译口令
	chatFrame := findFrameWith(popup, `textarea.chat-textarea`)
	if chatFrame == nil {
		// 尝试切到 CodeArts Web tab
		if clickAnyTextAcrossFrames(popup, []string{"CodeArts Web"}) {
			popup.WaitForTimeout(8000)
			chatFrame = findFrameWith(popup, `textarea.chat-textarea`)
		}
	}
	if chatFrame == nil {
		return nil, fmt.Errorf("找不到 CodeArts Web 输入框")
	}
	ta := chatFrame.Locator(`textarea.chat-textarea`).First()
	_ = ta.Click()
	prompt := fmt.Sprintf(buildPromptTpl, chosen.PublicRepo)
	_ = ta.PressSequentially(prompt, playwright.LocatorPressSequentiallyOptions{Delay: playwright.Float(5)})
	popup.WaitForTimeout(1000)
	if val, _ := ta.InputValue(); strings.TrimSpace(val) == "" {
		return nil, fmt.Errorf("编译口令未填入输入框")
	}
	_ = ta.Press("Enter")
	logStep("[cloud-practice] 已发送编译口令")
	out["prompt_sent"] = true

	// 7) 循环点授权 + 等编译成功（最多 12 分钟）
	compiled := false
	deadline := time.Now().Add(12 * time.Minute)
	lastLog := time.Now()
	for time.Now().Before(deadline) {
		popup.WaitForTimeout(8000)
		approveCount := 0
		// 点所有「始终允许」授权
		for _, f := range popup.Frames() {
			btn := f.Locator(`button.perm-btn.always`)
			n, _ := btn.Count()
			for i := 0; i < n; i++ {
				if err := btn.First().Click(playwright.LocatorClickOptions{Timeout: playwright.Float(3000)}); err == nil {
					approveCount++
				}
			}
		}
		// 检测编译成功
		for _, f := range popup.Frames() {
			txt, _ := f.Locator("body").InnerText()
			if strings.Contains(txt, "编译成功") || strings.Contains(txt, "BUILD SUCCESSFUL") ||
				(strings.Contains(txt, "HAP") && strings.Contains(txt, "产物")) {
				compiled = true
				break
			}
		}
		if compiled {
			break
		}
		if time.Since(lastLog) > 60*time.Second {
			logStep("[cloud-practice] 等待编译…（授权 %d 次）", approveCount)
			lastLog = time.Now()
		}
	}
	out["compiled"] = compiled
	if compiled {
		logStep("[cloud-practice] 编译打包成功")
	}

	// 8) 关机收尾（体验版本身免费，但持久化环境要关）
	shutdownOK := shutdownDevSpace(popup)
	out["shutdown"] = shutdownOK
	logStep("[cloud-practice] 关机: %v", shutdownOK)

	out["ok"] = compiled
	if !compiled {
		out["msg"] = "未在 12 分钟内检测到编译成功"
	} else {
		out["msg"] = "云端实践编译打包成功"
	}
	return out, nil
}

// createHarmonyProject 在项目管理面板新建项目（表单通常由案例链接自动填好）。
func createHarmonyProject(page playwright.Page) error {
	// 找到含「新建项目」的 frame
	var pf playwright.Frame
	for _, f := range page.Frames() {
		txt, _ := f.Locator("body").InnerText()
		if strings.Contains(txt, "新建项目") && strings.Contains(txt, "技术模板") {
			pf = f
			break
		}
	}
	if pf == nil {
		return fmt.Errorf("找不到项目管理面板")
	}
	// 点「新建项目」打开表单
	btn := pf.Locator(`button:has-text("新建项目")`).First()
	if n, _ := btn.Count(); n > 0 {
		_ = btn.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(6000)})
		page.WaitForTimeout(2000)
	}
	// 表单字段通常由案例链接预填；确认关键字段非空，空则填默认值
	nameIn := pf.Locator(`input[placeholder="请输入项目名称"]`).First()
	if n, _ := nameIn.Count(); n > 0 {
		v, _ := nameIn.InputValue()
		if strings.TrimSpace(v) == "" {
			_ = nameIn.Fill("HarmonyPractice")
		}
	}
	dirIn := pf.Locator(`input[placeholder*="my-app"]`).First()
	if n, _ := dirIn.Count(); n > 0 {
		v, _ := dirIn.InputValue()
		if strings.TrimSpace(v) == "" {
			_ = dirIn.Fill("Harmony-APP-practice")
		}
	}
	// 点「创建」
	create := pf.Locator(`button:has-text("创建")`).Last()
	if n, _ := create.Count(); n == 0 {
		return fmt.Errorf("找不到创建按钮")
	}
	if err := create.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(10000)}); err != nil {
		return err
	}
	page.WaitForTimeout(15000)
	return nil
}

// clickTextExact 在页面所有元素里点首个文字精确匹配 label 的元素。
func clickTextExact(page playwright.Page, label string) bool {
	for _, f := range page.Frames() {
		l := f.Locator(`text="` + label + `"`).First()
		if n, _ := l.Count(); n > 0 {
			if err := l.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err == nil {
				return true
			}
		}
	}
	return false
}

// clickAnyText 在页面里点首个包含任一文字的元素。
func clickAnyText(page playwright.Page, labels []string) bool {
	for _, f := range page.Frames() {
		for _, label := range labels {
			l := f.Locator(`text=` + label).First()
			if n, _ := l.Count(); n > 0 {
				if err := l.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(6000)}); err == nil {
					return true
				}
			}
		}
	}
	return false
}

// clickAnyTextAcrossFrames 同 clickAnyText，独立命名便于区分调用点。
func clickAnyTextAcrossFrames(page playwright.Page, labels []string) bool { return clickAnyText(page, labels) }

// findFrameWith 找包含指定 selector 的 frame。
func findFrameWith(page playwright.Page, selector string) playwright.Frame {
	for _, f := range page.Frames() {
		if n, _ := f.Locator(selector).Count(); n > 0 {
			return f
		}
	}
	return nil
}

// shutdownDevSpace 关闭 DevSpace 环境，避免空转持续消耗核时额度。
func shutdownDevSpace(page playwright.Page) bool {
	defer func() { _ = recover() }()
	for _, f := range page.Frames() {
		btn := f.Locator(`[title="关机"]`)
		n, _ := btn.Count()
		if n == 0 {
			continue
		}
		if vis, _ := btn.First().IsVisible(); !vis {
			continue
		}
		_ = btn.First().Click(playwright.LocatorClickOptions{Timeout: playwright.Float(6000)})
		page.WaitForTimeout(1500)
		for _, label := range []string{"确认", "确定"} {
			cf := f.Locator(`.shutdown-confirm-dialog button:has-text("` + label + `")`)
			if cn, _ := cf.Count(); cn > 0 {
				if vis, _ := cf.First().IsVisible(); vis {
					if err := cf.First().Click(playwright.LocatorClickOptions{Timeout: playwright.Float(6000)}); err == nil {
						page.WaitForTimeout(3000)
						return true
					}
				}
			}
		}
	}
	return false
}

// pickFreeDevSpaceLink 从实践中心页面挑免费 DevSpace 入口（避开带 dc_sign 的收费链接）。
func pickFreeDevSpaceLink(page playwright.Page) playwright.Locator {
	all := page.Locator(`a:has-text("前往AI DevSpace实践")`)
	n, _ := all.Count()
	for i := 0; i < n; i++ {
		el := all.Nth(i)
		href, _ := el.GetAttribute("href")
		if href != "" && strings.Contains(href, "officeagent") {
			return el
		}
	}
	return all.Last()
}

// harmonyPracticeCase 案例列表条目（从案例页 DOM 抓）。
type harmonyPracticeCase struct {
	ID         string
	Title      string
	PublicRepo string
}

// pickNextCase 从案例列表页挑一个与上次不同的案例。
// 优先标题含「数独/入门/纯端侧」的（编译快），否则取第一个不同于上次的。
func pickNextCase(page playwright.Page) (harmonyPracticeCase, error) {
	lastID := readLastCase()
	raw, err := page.Evaluate(`() => {
		const out = [];
		for (const a of document.querySelectorAll('a[href]')) {
			const m = (a.getAttribute('href')||'').match(/\/cases\/([0-9a-f]{32})/);
			if (!m) continue;
			let card = a;
			for (let i=0;i<5 && card;i++) card = card.parentElement || card;
			const title = (card ? card.innerText : '').split('\n')[0].trim().slice(0,120);
			out.push({id:m[1], title:title});
		}
		const seen = new Set();
		return out.filter(x=>{if(seen.has(x.id))return false;seen.add(x.id);return true;});
	}`)
	if err != nil {
		return harmonyPracticeCase{}, err
	}
	items, _ := raw.([]any)
	if len(items) == 0 {
		return harmonyPracticeCase{}, fmt.Errorf("案例列表为空")
	}
	var candidates []harmonyPracticeCase
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m == nil { continue }
		id, _ := m["id"].(string)
		title, _ := m["title"].(string)
		if id == "" { continue }
		candidates = append(candidates, harmonyPracticeCase{ID: id, Title: title})
	}
	if len(candidates) == 0 {
		return harmonyPracticeCase{}, fmt.Errorf("未解析到案例")
	}
	// 优先：不同于上次 + 标题含快速关键词
	for _, c := range candidates {
		if c.ID == lastID { continue }
		if strings.Contains(c.Title, "数独") || strings.Contains(c.Title, "入门") || strings.Contains(c.Title, "纯端侧") {
			c.PublicRepo = publicRepoForCase(c.ID, c.Title)
			return c, nil
		}
	}
	// 兜底：第一个不同于上次的
	for _, c := range candidates {
		if c.ID == lastID { continue }
		c.PublicRepo = publicRepoForCase(c.ID, c.Title)
		return c, nil
	}
	// 只有一个案例（跟上次相同），仍然用它
	c := candidates[0]
	c.PublicRepo = publicRepoForCase(c.ID, c.Title)
	return c, nil
}

// publicRepoForCase 根据案例构造公仓 URL。
// 已知案例用映射；未知案例走通用命名 harmony-practice-center/Harmony-APP-<关键词>。
func publicRepoForCase(id, title string) string {
	known := map[string]string{
		"c0043e1e9d0240d19c23fa2556d5ecfc": "https://gitcode.com/harmony-practice-center/Harmony-APP-sudoku.git",
	}
	if u, ok := known[id]; ok { return u }
	// 通用：从详情页拿（这里先给占位，后续可由详情页「案例公仓」字段覆盖）
	return "https://gitcode.com/harmony-practice-center/Harmony-APP-" + id[:8] + ".git"
}

func lastCasePath() string { return filepath.Join(config.DataDir(), lastCaseFile) }

func readLastCase() string {
	b, err := os.ReadFile(lastCasePath())
	if err != nil { return "" }
	return strings.TrimSpace(string(b))
}

func saveLastCase(id string) {
	_ = os.MkdirAll(filepath.Dir(lastCasePath()), 0o755)
	_ = os.WriteFile(lastCasePath(), []byte(id), 0o644)
	logStep("[cloud-practice] 已记录本次案例 %s", id)
}

// clipRunes 按 rune 截断，避免中文被截成半个字。
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
