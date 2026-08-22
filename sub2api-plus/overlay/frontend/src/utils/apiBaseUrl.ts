export function parseApiBaseUrls(value: string | null | undefined): string[] {
  const seen = new Set<string>()
  const urls: string[] = []

  for (const item of String(value || '').split(';')) {
    const trimmed = item.trim()
    if (!trimmed || seen.has(trimmed)) continue
    seen.add(trimmed)
    urls.push(trimmed)
  }

  return urls
}

export function formatApiBaseUrls(value: string | null | undefined): string {
  return parseApiBaseUrls(value).join(';')
}

export function getPrimaryApiBaseUrl(value: string | null | undefined, fallback: string): string {
  return parseApiBaseUrls(value)[0] || fallback
}

export function formatApiBaseUrlLabel(value: string): string {
  return value.replace(/^[a-z][a-z\d+.-]*:\/\//i, '')
}
