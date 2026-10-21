package upstream

import (
	"regexp"
	"strings"
)

// 上游模型目录的 name 是**模型家族名**（如 "Deepseek-V4.1-Flash"），不带小版本；
// 小版本号只出现在 description 的更新说明里，目前线上形态是：
//
//	"已更新至 V4.1-Flash-1017最新版，支持1M上下文窗口，原生多模态"
//
// 也就是「已更新至」+ 模型家族-版本号 + 「最新版」。客户端/面板只看 name，
// 于是新版本上线后界面上的名字毫无变化，用户无法判断自己用的是哪一版。
//
// 这里把版本号从描述里择出来，供展示层拼到 name 后面（"Deepseek-V4.1-Flash1017"）。
//
// **只用于展示，绝不参与路由/白名单/匹配**：模型的 id 一个字都不改，客户端
// 按 id 调用照旧；白名单、渠道分流、effort 查找全部仍以 id 为准。
var (
	// modelVersionLeadRe 定位更新说明：中文「已更新至/更新至/已升级至/升级至」，
	// 英文「updated to」。必须命中这个前缀才算数——描述里还有「支持1M上下文窗口」
	// 这类裸数字，只按数字猜版本会把它们误当版本号。
	modelVersionLeadRe = regexp.MustCompile(`(?i)(?:已更新至|更新至|已升级至|升级至|updated to)\s*([^\s，。；;,]{2,64})`)

	// modelVersionTailRe 从上面抓到的片段里取**最后一段** "-数字"：
	// "V4.1-Flash-1017最新版" → 1017（"V4.1" 和 "Flash" 都不匹配，因为要求
	// 数字后面只剩非数字字符到结尾）。2~8 位，避免把 "x0.11" 之类当版本。
	modelVersionTailRe = regexp.MustCompile(`[-_ ](\d{2,8})\D*$`)
)

// ModelVersion 从 description 里提取上游声明的小版本号；没有则返回空串。
// 认不出就返回空——宁可界面不带版本，也不能瞎编一个版本号。
func (m ModelInfo) ModelVersion() string {
	if m.Description == "" {
		return ""
	}
	lead := modelVersionLeadRe.FindStringSubmatch(m.Description)
	if lead == nil {
		return ""
	}
	tail := modelVersionTailRe.FindStringSubmatch(lead[1])
	if tail == nil {
		return ""
	}
	return tail[1]
}

// DisplayName 展示用模型名：家族名 + 版本号（"Deepseek-V4.1-Flash" + "1017"）。
// 无版本号 / 名字为空 / 名字已自带该版本号 → 原样返回，绝不重复拼接。
//
// 调用方只能把它写进展示字段（/v1/models 的 name、面板模型表的 name），
// 不能写进 id 或任何用于匹配的字段。
func (m ModelInfo) DisplayName() string {
	v := m.ModelVersion()
	if v == "" || m.Name == "" || strings.HasSuffix(m.Name, v) {
		return m.Name
	}
	return m.Name + v
}
