/**
 * 验证并规范化 URL
 * 默认只接受绝对 URL（以 http:// 或 https:// 开头），可按需允许相对路径
 * @param value 用户输入的 URL
 * @returns 规范化后的 URL，如果无效则返回空字符串
 */
type SanitizeOptions = {
  allowRelative?: boolean
  allowDataUrl?: boolean
}

export function sanitizeUrl(value: string, options: SanitizeOptions = {}): string {
  const trimmed = value.trim()
  if (!trimmed) {
    return ''
  }

  if (options.allowRelative && trimmed.startsWith('/') && !trimmed.startsWith('//')) {
    return trimmed
  }

  // 允许 data:image/ 开头的 data URL（仅限图片类型）
  if (options.allowDataUrl && trimmed.startsWith('data:image/')) {
    return trimmed
  }

  // 只接受绝对 URL，不使用 base URL 来避免相对路径被解析为当前域名
  // 检查是否以 http:// 或 https:// 开头
  if (!trimmed.match(/^https?:\/\//i)) {
    return ''
  }

  try {
    const parsed = new URL(trimmed)
    const protocol = parsed.protocol.toLowerCase()
    if (protocol !== 'http:' && protocol !== 'https:') {
      return ''
    }
    return parsed.toString()
  } catch {
    return ''
  }
}

/**
 * 把一段可能含 URL 的纯文本切成「文本段 / 链接段」，供模板用 <template> 渲染，
 * 不走 v-html，天然免疫 XSS。链接段再经 sanitizeUrl 白名单校验（仅 http/https），
 * 非法 URL 退回当作普通文本。
 *
 * 例：'Telegram 群 znbcode: https://t.me/+abc'
 *   → [{text:'Telegram 群 znbcode: '}, {url:'https://t.me/+abc', label:'https://t.me/+abc'}]
 */
export interface ContactSegment {
  text?: string
  url?: string
  label?: string
}

export function linkifyContact(value: string): ContactSegment[] {
  const input = (value ?? '').trim()
  if (!input) {
    return []
  }
  const segments: ContactSegment[] = []
  // 匹配 http(s):// 开头、到空白/引号/尖括号为止的一串
  const re = /https?:\/\/[^\s<>"']+/gi
  let last = 0
  let m: RegExpExecArray | null
  while ((m = re.exec(input)) !== null) {
    if (m.index > last) {
      segments.push({ text: input.slice(last, m.index) })
    }
    const raw = m[0]
    const safe = sanitizeUrl(raw)
    if (safe) {
      segments.push({ url: safe, label: raw })
    } else {
      segments.push({ text: raw }) // 校验不过就当普通文本
    }
    last = m.index + raw.length
  }
  if (last < input.length) {
    segments.push({ text: input.slice(last) })
  }
  return segments
}
