import { describe, expect, it } from 'vitest'
import { formatApiBaseUrlLabel, formatApiBaseUrls, getPrimaryApiBaseUrl, parseApiBaseUrls } from '@/utils/apiBaseUrl'

describe('apiBaseUrl utils', () => {
  it('parses semicolon-separated API base URLs with trimming and dedupe', () => {
    expect(parseApiBaseUrls(' https://a.example.com ; ;https://b.example.com;https://a.example.com ')).toEqual([
      'https://a.example.com',
      'https://b.example.com',
    ])
  })

  it('formats API base URLs back into the legacy semicolon-separated storage string', () => {
    expect(formatApiBaseUrls(' https://a.example.com ; https://b.example.com ')).toBe(
      'https://a.example.com;https://b.example.com'
    )
  })

  it('returns the first API base URL with a fallback', () => {
    expect(getPrimaryApiBaseUrl('https://a.example.com;https://b.example.com', 'https://fallback.example.com')).toBe(
      'https://a.example.com'
    )
    expect(getPrimaryApiBaseUrl('', 'https://fallback.example.com')).toBe('https://fallback.example.com')
  })

  it('formats API base URLs for compact selector labels', () => {
    expect(formatApiBaseUrlLabel('https://a.example.com')).toBe('a.example.com')
    expect(formatApiBaseUrlLabel('http://b.example.com/v1')).toBe('b.example.com/v1')
  })
})
