// desensitize.go 实现日志内容的安全脱敏处理。
//
// 本文件对编码工具的输出进行敏感信息过滤，防止凭据泄露到服务端，主要包括：
//   - DesensitizeLog：对完整日志内容进行脱敏，用于批量日志上传
//   - desensitizeOutputLine：对单行实时输出进行脱敏，用于 SSE 流式传输
//   - sensitivePatterns：预编译正则模式，覆盖 API 密钥、Bearer 令牌、AWS 密钥等格式
//
// 脱敏后的值替换为 ***REDACTED***，保留 key/value 前缀以便调试。
package agent

import (
	"regexp"
)

// sensitivePatterns 匹配日志输出中的常见密钥格式，用于脱敏处理。
// 包含 API 密钥、Bearer 令牌、通用 key=value 模式、AWS 密钥和十六进制令牌等格式。
var sensitivePatterns = []*regexp.Regexp{
	// API 密钥格式：sk-..., ghp_..., tm_... 等
	regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9]{20,})`),
	regexp.MustCompile(`(?i)(ghp_[a-zA-Z0-9]{20,})`),
	regexp.MustCompile(`(?i)(tm_[a-zA-Z0-9_]{20,})`),
	// Bearer 令牌格式
	regexp.MustCompile(`(?i)(Bearer\s+)(\S+)`),
	// 通用 key=value 模式
	regexp.MustCompile(`(?i)(api[_-]?key|token|password|secret|credential|auth[_-]?token|access[_-]?key|private[_-]?key)(\s*[=:]\s*)\S+`),
	// AWS 风格的密钥格式
	regexp.MustCompile(`(?i)(AKIA[A-Z0-9]{16})`),
	// 通用十六进制令牌格式（已知前缀后的 16+ 十六进制字符）
	regexp.MustCompile(`(?i)(["']?[a-zA-Z0-9._-]*(?:key|token|secret|password|credential|pat)["']?\s*[=:]\s*["']?)([a-zA-Z0-9+/=_-]{16,})`),
}

// DesensitizeLog 对日志内容进行脱敏处理，将 API 密钥、令牌、密码等凭据替换为 ***REDACTED***。
// 用于日志内容上传到服务端前的安全处理。
//
// 参数：
//   - content: 原始日志内容
//
// 返回：
//   - string: 脱敏后的日志内容
func DesensitizeLog(content string) string {
	result := content
	for _, pat := range sensitivePatterns {
		result = pat.ReplaceAllStringFunc(result, func(match string) string {
			// 对于有捕获组的模式，保留前缀并替换值部分
			groups := pat.FindStringSubmatch(match)
			if len(groups) >= 3 {
				return groups[1] + "***REDACTED***"
			}
			return "***REDACTED***"
		})
	}
	return result
}

// desensitizeOutputLine 对单行输出进行脱敏处理，用于实时输出流。
//
// 参数：
//   - line: 单行输出内容
//
// 返回：
//   - string: 脱敏后的内容
func desensitizeOutputLine(line string) string {
	for _, pat := range sensitivePatterns {
		line = pat.ReplaceAllStringFunc(line, func(match string) string {
			groups := pat.FindStringSubmatch(match)
			if len(groups) >= 3 {
				return groups[1] + "***REDACTED***"
			}
			return "***REDACTED***"
		})
	}
	return line
}
