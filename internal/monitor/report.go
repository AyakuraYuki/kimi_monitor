package monitor

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"kimi_balance_monitor/internal/config"
	"kimi_balance_monitor/internal/model"
	"kimi_balance_monitor/internal/wecom"
)

// BuildMessages 生成整体报告；超出企业微信限制时按完整令牌块拆分。
func BuildMessages(results []model.TokenResult, checkedAt time.Time, lowThresholdMicros int64, consumption model.ConsumptionSummary) []string {
	header := buildHeader("## Kimi 令牌余额监控", results, checkedAt, lowThresholdMicros)
	continuationHeader := buildHeader("## Kimi 令牌余额监控（续）", results, checkedAt, lowThresholdMicros)
	footer := buildConsumptionFooter(consumption, checkedAt)
	if len(results) == 0 {
		return []string{strings.TrimSpace(header + "\n暂无已配置的令牌。\n\n" + footer)}
	}

	blocks := make([]string, 0, len(results))
	for _, result := range results {
		blocks = append(blocks, buildTokenBlock(result, lowThresholdMicros))
	}
	messages := make([]string, 0, 1)
	current := header
	hasBlocks := false
	for _, block := range blocks {
		if hasBlocks && len(current)+len(block) > wecom.MaxMarkdownBytes {
			messages = append(messages, strings.TrimSpace(current))
			current = continuationHeader
			hasBlocks = false
		}
		if len(current)+len(block) > wecom.MaxMarkdownBytes {
			block = truncateUTF8(block, wecom.MaxMarkdownBytes-len(current))
		}
		current += block
		hasBlocks = true
	}
	if hasBlocks && len(current)+len(footer) > wecom.MaxMarkdownBytes {
		messages = append(messages, strings.TrimSpace(current))
		current = continuationHeader
	}
	current += footer
	messages = append(messages, strings.TrimSpace(current))
	return messages
}

func buildConsumptionFooter(consumption model.ConsumptionSummary, checkedAt time.Time) string {
	localTime := checkedAt.In(config.CST())
	var builder strings.Builder
	_, _ = fmt.Fprintf(
		&builder,
		"### 消耗统计（北京时间）\n> 今日累计消耗（00:00 至 %s）：**%s**\n",
		localTime.Format("15:04:05"),
		model.FormatCNY(consumption.TodayMicros),
	)
	if localTime.Hour() == 0 {
		_, _ = fmt.Fprintf(
			&builder,
			"> 昨日总消耗（%s）：**%s**\n",
			localTime.AddDate(0, 0, -1).Format(time.DateOnly),
			model.FormatCNY(consumption.YesterdayMicros),
		)
	}
	return builder.String()
}

func buildHeader(title string, results []model.TokenResult, checkedAt time.Time, lowThresholdMicros int64) string {
	var (
		normal         int
		low            int
		exhausted      int
		failed         int
		replenished    int
		totalAvailable int64
		statusParts    []string
		availableParts []string
	)
	for _, result := range results {
		switch result.Health(lowThresholdMicros) {
		case model.HealthNormal:
			normal++
		case model.HealthLow:
			low++
		case model.HealthExhausted:
			exhausted++
		case model.HealthError:
			failed++
		}
		if result.Change() == model.ChangeReplenished {
			replenished++
		}
		if result.Err == nil && result.Current != nil {
			totalAvailable += result.Current.Balance.AvailableMicros
		}
	}

	statusParts = append(statusParts, fmt.Sprintf("🟢 正常 %d", normal))
	if low > 0 {
		statusParts = append(statusParts, fmt.Sprintf("🟡 预警 %d", low))
	}
	if exhausted > 0 {
		statusParts = append(statusParts, fmt.Sprintf("🔴 用尽 %d", exhausted))
	}
	if failed > 0 {
		statusParts = append(statusParts, fmt.Sprintf("⚠️ 异常 %d", failed))
	}

	availableParts = append(availableParts, fmt.Sprintf("可用余额合计：**%s**", model.FormatCNY(totalAvailable)))
	if replenished > 0 {
		availableParts = append(availableParts, fmt.Sprintf("本轮补充：%d 个", replenished))
	}

	return fmt.Sprintf(`%s
> 检查时间：%s
> 令牌：%d 个 ｜ %s
> %s
> 低余额预警线：%s

`,
		title,
		checkedAt.In(config.CST()).Format(time.DateTime),
		len(results),
		strings.Join(statusParts, " | "),
		strings.Join(availableParts, " | "),
		model.FormatCNY(lowThresholdMicros))
}

func buildTokenBlock(result model.TokenResult, lowThresholdMicros int64) string {
	var builder strings.Builder
	_, _ = fmt.Fprintf(&builder, "### #%d `%s`\n", result.TokenID, result.Name)

	if remark := strings.TrimSpace(result.Remark); remark != "" {
		_, _ = fmt.Fprintf(&builder, "> 备注：%s\n", markdownText(remark))
	}

	if result.Health(lowThresholdMicros) == model.HealthError {
		builder.WriteString("<font color=\"warning\">⚠️ 查询异常</font>\n")
		_, _ = fmt.Fprintf(&builder, "> 错误：%s\n", markdownText(errorMessage(result.Err)))
		builder.WriteString("> 本轮未生成余额快照；后续环比仍使用最近一次成功记录。\n\n")
		return builder.String()
	}

	var (
		status  = statusText(result.Health(lowThresholdMicros))
		balance = model.FormatCNY(result.Current.Balance.AvailableMicros)
	)

	if result.Change() == model.ChangeUnchanged {
		_, _ = fmt.Fprintf(&builder, "%s　可用余额：**%s**\n\n", status, balance)
		return builder.String()
	}

	_, _ = fmt.Fprintf(&builder, "%s　%s\n", status, changeText(result))
	_, _ = fmt.Fprintf(&builder, "> 可用余额：**%s**\n", balance)

	if result.Previous != nil {
		var (
			available = model.FormatCNY(result.Previous.Balance.AvailableMicros)
			checkedAt = result.Previous.CheckedAt.In(config.CST()).Format(time.DateTime)
		)
		_, _ = fmt.Fprintf(&builder, "> 上次可用：%s（%s）\n", available, checkedAt)
	}

	var (
		cash = model.FormatCNY(result.Current.Balance.CashMicros)
	)
	if result.Current.Balance.VoucherMicros > 0 {
		_, _ = fmt.Fprintf(&builder, "> 现金：%s | 代金券：%s\n\n", cash, model.FormatCNY(result.Current.Balance.VoucherMicros))
	} else {
		_, _ = fmt.Fprintf(&builder, "> 现金：%s\n\n", cash)
	}

	return builder.String()
}

func statusText(health model.Health) string {
	switch health {
	case model.HealthNormal:
		return `<font color="info">🟢 正常</font>`
	case model.HealthLow:
		return `<font color="warning">🟡 余额不足</font>`
	case model.HealthExhausted:
		return `<font color="warning">🔴 已用尽</font>`
	default:
		return `<font color="warning">⚠️ 查询异常</font>`
	}
}

func changeText(result model.TokenResult) string {
	switch result.Change() {
	case model.ChangeFirst:
		return "🆕 首次记录，暂无环比"
	case model.ChangeUnchanged:
		return "➖ 较上次无变化"
	case model.ChangeReplenished:
		delta, _ := result.DeltaMicros()
		suffix := ""
		if result.Current.Balance.AvailableMicros <= 0 {
			suffix = "（已补充，但仍不可用）"
		} else if result.Previous.Balance.AvailableMicros <= 0 {
			suffix = "（已恢复可用）"
		}
		return fmt.Sprintf("📈 补充 **%s**%s", model.FormatCNY(delta), suffix)
	case model.ChangeConsumption:
		delta, _ := result.DeltaMicros()
		suffix := ""
		if result.Current.Balance.AvailableMicros <= 0 && result.Previous.Balance.AvailableMicros > 0 {
			suffix = "（本轮耗尽）"
		}
		return fmt.Sprintf("📉 消耗 **%s**%s", model.FormatCNY(-delta), suffix)
	default:
		return "环比不可用"
	}
}

func errorMessage(err error) string {
	if err == nil {
		return "未生成余额快照"
	}
	return err.Error()
}

func markdownText(text string) string {
	text = strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"`", "'",
		"\r", " ",
		"\n", " ",
	).Replace(text)
	runes := []rune(text)
	if len(runes) > 180 {
		return string(runes[:180]) + "……"
	}
	return text
}

func truncateUTF8(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	const suffix = "\n> ……（内容已截断）\n"
	if maxBytes <= len(suffix) {
		return "……"
	}
	end := maxBytes - len(suffix)
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end] + suffix
}
