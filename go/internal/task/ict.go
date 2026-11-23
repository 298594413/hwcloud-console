package task

import (
	"fmt"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"

	"hwcloud/internal/config"
)

// ICT 大赛创新赛报名（新任务 +350，一次性）。
//
// 新任务说明（2026-11-23）：登录 ICT 大赛平台，报名创新赛，绑定华为云账号，
// 完成实名认证，查看报名信息，并开通华为云码道。
//
// 这条链路依赖 ICT 平台自己的账号体系与多步表单（实名、绑定、开通码道），
// 在没有确认各步 DOM 之前不做盲点。这里只做**只读探测**，并把需要人工
// 介入的点明确返回，绝不返回假成功。
const ictInnovationURL = "https://gallery.developer.huaweicloud.com/gallery/competition?type=innovation"

// ICTInnovationStatus 只读探测 ICT 创新赛报名入口。
func ICTInnovationStatus(headless bool) (map[string]any, error) {
	out := map[string]any{"url": ictInnovationURL, "ok": false}
	acct := config.AccountID()
	profile, port := galleryProfileFor(acct)
	if err := ensureGalleryBrowser(profile, port); err != nil {
		out["msg"] = "浏览器启动失败: " + err.Error()
		return out, nil
	}
	defer closeGalleryBrowser(port)

	pw, err := playwright.Run()
	if err != nil {
		out["msg"] = "Playwright 启动失败: " + err.Error()
		return out, nil
	}
	defer pw.Stop()
	br, err := pw.Chromium.ConnectOverCDP("http://127.0.0.1:" + port)
	if err != nil {
		out["msg"] = "CDP 连接失败: " + err.Error()
		return out, nil
	}
	defer br.Close()
	if len(br.Contexts()) == 0 {
		out["msg"] = "浏览器没有可用上下文"
		return out, nil
	}
	ctx := br.Contexts()[0]
	if err := seedHuaweiCookies(ctx); err != nil {
		out["msg"] = "注入华为云会话失败: " + err.Error()
		return out, nil
	}
	page, err := ctx.NewPage()
	if err != nil {
		out["msg"] = "新建页面失败: " + err.Error()
		return out, nil
	}
	defer page.Close()
	if _, err := page.Goto(ictInnovationURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
		Timeout:   playwright.Float(45000),
	}); err != nil {
		out["msg"] = "打开 ICT 赛事页失败: " + err.Error()
		return out, nil
	}
	time.Sleep(5 * time.Second)
	if title, err := page.Title(); err == nil {
		out["page_title"] = title
	}
	if txt, err := page.InnerText("body"); err == nil {
		s := strings.TrimSpace(txt)
		if r := []rune(s); len(r) > 500 {
			s = string(r[:500]) + "…"
		}
		out["page_text"] = s
		// 判定登录态必须看「已登录」正向标记，不能用「出现登录字样」。
		// 实测页面正文同时包含「登录华为云账号 ✓ 已登录（hid_xxx）」和
		// 标题里的「登录」，用 Contains("登录") 会永远误判成未登录。
		logged := strings.Contains(txt, "已登录")
		out["logged_in"] = logged
		if !logged {
			out["msg"] = "赛事页显示未登录，需要先恢复华为云会话"
			return out, nil
		}
		// 已登录：识别当前处于哪一步，供人工/后续自动化使用。
		switch {
		case strings.Contains(txt, "实名认证") && strings.Contains(txt, "授权并领取积分"):
			out["step_hint"] = "需完成实名认证并授权领取积分"
		case strings.Contains(txt, "下一步"):
			out["step_hint"] = "登录已就绪，可点下一步继续报名"
		}
	}
	out["msg"] = "赛事页已登录；报名 / 实名 / 码道开通涉及多步表单，未自动提交"
	return out, nil
}

var _ = fmt.Sprintf
