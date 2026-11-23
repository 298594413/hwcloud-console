package task

import (
	"fmt"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/browser"
)

// 鸿蒙实践中心的「云端实践」(+100，每周)。
//
// 为什么纯协议走不通：
//   evaluator 是 external_report，`submit` 接口直接 403
//   `browser_submit_not_allowed` —— 上报由 DevSpace 环境内部完成，
//   不对外暴露提交口。
//
// 完整链路（全部在浏览器里完成）：
//  1. 打开 devstation 的 harmony-practice-center 页，让它走
//     index/islogin?ticket=ST-... 完成 DevSpace 侧登录
//  2. 点「临时凭据自动配置」弹窗的「同意」—— 不点会一直卡在这个弹窗，
//     永远进不了环境
//  3. 环境创建/启动（容器 devenv），等「环境准备中」消失
//  4. 环境就绪后 floor-practice-center 会加载，上报由平台侧自己发
//
// 关于内测资格：
//   案例详情页的「云端实践」按钮会先查 beta-harmony-practice-application，
//   但那个接口在 devstation-svc.connect 域，用 Go 发要过 WAF + CSRF，
//   实测拿不到登录态（403 not login）。好在这不影响主流程 ——
//   直接打开 DevSpace 页面本身就会走完整登录并创建环境。
//   申请状态只是给人看的信息，不是执行前提。
//
// 注意：这个函数会真的创建/启动一个 DevSpace 容器环境，消耗核时。
// 调用前应先确认任务未完成。

const (
	devSpacePageURL = "https://devstation.developer.huaweicloud.com/ai-devspace/beta/harmony-practice-center"
)

// RunCloudPractice 完成「云端实践」：打开 DevSpace → 授权 → 等环境就绪。
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

	if _, err := page.Goto(devSpacePageURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(90000),
	}); err != nil {
		return nil, fmt.Errorf("打开 DevSpace 失败: %v", err)
	}
	page.WaitForTimeout(16000)

	// 点「同意」授权临时凭据。
	//
	// 这个弹窗是进入环境的必经步骤：不点，页面就一直停在
	// 「是否同意将您的临时访问凭据(AK/SK) 同步至 AI 开发者空间环境」，
	// 环境根本不会开始创建。
	agreed := false
	// 2026-11-23 实测：新版页面会弹「CodeArts代码智能体服务声明」，
	// 按钮是「立即开通」，不点就一直卡在「环境准备中」。加入匹配。
	for _, label := range []string{"同意", "同意并继续", "立即开通"} {
		for _, sel := range []string{
			`button:has-text("%s")`,
			`.el-button:has-text("%s")`,
			`[class*="dialog"] button:has-text("%s")`,
			`[class*="modal"] button:has-text("%s")`,
		} {
			loc := page.Locator(fmt.Sprintf(sel, label))
			n, _ := loc.Count()
			if n == 0 {
				continue
			}
			if vis, _ := loc.First().IsVisible(); !vis {
				continue
			}
			if err := loc.First().Click(playwright.LocatorClickOptions{
				Timeout: playwright.Float(6000),
			}); err == nil {
				agreed = true
				break
			}
		}
		if agreed {
			break
		}
	}

	// 等环境就绪。
	// 判据：「环境准备中」消失，且出现「剩余核时」或「当前环境运行中」。
	//
	// 2026-11-23 实测（周常真实失败原因）：
	//   环境其实在正常创建，但 DevSpace 弹出了「版本有更新，建议升级」对话框，
	//   正文是「环境准备中，请勿关闭窗口...」+ 升级弹窗的「确认/取消」。
	//   这个弹窗不处理会一直盖住页面，等待循环只能看到「环境准备中」，
	//   最后误报成「环境 12 分钟未就绪」。
	//   所以每轮先尝试关掉「取消」/「稍后」/「关闭」，再判断状态。
	ready := false
	deadline := time.Now().Add(12 * time.Minute)
	lastText := ""
	dismissedUpgrade := false
	for time.Now().Before(deadline) {
		page.WaitForTimeout(6000)
		// 处理升级弹窗：点「取消」比点「确认」安全（不在任务里触发版本升级）。
		if !dismissedUpgrade {
			for _, label := range []string{"取消", "稍后", "以后再说", "关闭"} {
				for _, sel := range []string{
					`[class*="dialog"] button:has-text("%s")`,
					`[class*="modal"] button:has-text("%s")`,
					`button:has-text("%s")`,
				} {
					loc := page.Locator(fmt.Sprintf(sel, label))
					if n, _ := loc.Count(); n == 0 {
						continue
					}
					if vis, _ := loc.First().IsVisible(); !vis {
						continue
					}
					if err := loc.First().Click(playwright.LocatorClickOptions{Timeout: playwright.Float(4000)}); err == nil {
						dismissedUpgrade = true
						time.Sleep(2 * time.Second)
						break
					}
				}
				if dismissedUpgrade {
					break
				}
			}
		}
		b, err := page.Locator("body").InnerText()
		if err != nil {
			continue
		}
		lastText = b
		if strings.Contains(b, "环境准备中") {
			continue
		}
		if strings.Contains(b, "剩余核时") || strings.Contains(b, "当前环境运行中") {
			ready = true
			break
		}
	}

	// 环境就绪后再停一会儿，让 floor-practice-center 加载并触发上报
	if ready {
		page.WaitForTimeout(20000)
	}

	// 上报触发后立即关机，避免环境空转持续消耗核时额度。
	// 关机按钮没有文字，只有 title="关机"；点后弹「是否确定关机？」需点「确认」。
	shutdownOK := shutdownDevSpace(page)
	logStep("[cloud-practice] 关机: %v", shutdownOK)

	msg := "环境未在 12 分钟内就绪（DevSpace 侧较慢，可稍后重试）"
	if ready {
		msg = "环境已就绪，云端实践已触发"
	}
	out := map[string]any{
		"ok":               ready,
		"agreed":           agreed,
		"env_ready":        ready,
		"dismissed_upgrade": dismissedUpgrade,
		"shutdown":         shutdownOK,
		"msg":              msg,
	}
	if !ready && lastText != "" {
		out["page_hint"] = clipRunes(lastText, 300)
	}
	return out, nil
}

// shutdownDevSpace 关闭 DevSpace 环境，避免空转持续消耗核时额度。
// 关机按钮没有文字，只有 title="关机"；点后弹「是否确定关机？」需点「确认」。
func shutdownDevSpace(page playwright.Page) bool {
	defer func() { _ = recover() }() // 关机失败不影响主流程结果
	for _, f := range page.Frames() {
		btn := f.Locator(`[title="关机"]`)
		n, _ := btn.Count()
		if n == 0 {
			continue
		}
		if vis, _ := btn.First().IsVisible(); !vis {
			continue
		}
		if err := btn.First().Click(playwright.LocatorClickOptions{Timeout: playwright.Float(6000)}); err != nil {
			// 可能确认框已弹出挡住按钮，直接走确认逻辑
			logStep("  点击关机按钮: %v（尝试直接确认）", err)
		}
		page.WaitForTimeout(1500)
		// 确认弹窗：是否确定关机？ 取消 / 确认
		for _, label := range []string{"确认", "确定"} {
			cf := f.Locator(`.shutdown-confirm-dialog button:has-text("` + label + `")`)
			if cn, _ := cf.Count(); cn > 0 {
				if vis, _ := cf.First().IsVisible(); vis {
					if err := cf.First().Click(playwright.LocatorClickOptions{Timeout: playwright.Float(6000)}); err == nil {
						page.WaitForTimeout(3000)
						logStep("  已确认关机")
						return true
					}
				}
			}
		}
		// 兜底：任意确认框里的确认按钮
		cf := f.Locator(`[class*="confirm"] button:has-text("确认")`)
		if cn, _ := cf.Count(); cn > 0 {
			if err := cf.First().Click(playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err == nil {
				page.WaitForTimeout(3000)
				return true
			}
		}
	}
	return false
}

// pickFreeDevSpaceLink 从实践中心页面挑「免费」的前往 DevSpace 入口。
// 页面上有多个同名链接：第一个带 dc/dc_sign 签名会进收费的 harmony-practice-center
// （创建容器、持续烧核时）；带 utm_source=officeagent 的是免费体验入口。
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
	return all.Last() // 兜底：最后一个通常也是 officeagent，避开第一个带签名的
}

// clipRunes 按 rune 截断，避免中文被截成半个字。
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
