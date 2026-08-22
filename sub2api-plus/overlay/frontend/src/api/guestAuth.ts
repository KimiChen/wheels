export interface GuestUser {
  id: number
  username: string
  email: string
  role: 'admin' | 'user' | string
  status?: string
  balance?: number
  [key: string]: unknown
}

export interface GuestAuthResponse {
  access_token: string
  refresh_token?: string
  expires_in?: number
  token_type: string
  user: GuestUser
}

type GuestLoginResponseData = GuestAuthResponse | Record<string, unknown>

export interface GuestRegisterRequest {
  email: string
  password: string
  verify_code?: string
  turnstile_token?: string
  promo_code?: string
  invitation_code?: string
  aff_code?: string
}

export interface GuestValidatePromoCodeResponse {
  valid: boolean
  bonus_amount?: number
  error_code?: string
  message?: string
}

export interface GuestValidateInvitationCodeResponse {
  valid: boolean
  error_code?: string
}

const AUTH_TOKEN_KEY = 'auth_token'
const AUTH_USER_KEY = 'auth_user'
const REFRESH_TOKEN_KEY = 'refresh_token'
const TOKEN_EXPIRES_AT_KEY = 'token_expires_at'

async function guestPost<T>(url: string, body: unknown): Promise<T> {
  let response: Response
  try {
    response = await fetch(url, {
      method: 'POST',
      credentials: 'include',
      headers: {
        'Content-Type': 'application/json',
        'Accept-Language': 'zh',
      },
      body: JSON.stringify(body),
    })
  } catch {
    throw { status: 0, message: '网络连接异常，请稍后重试。' }
  }

  const payload = await readJson(response)
  if (!response.ok) {
    throw normalizeGuestError(response.status, payload)
  }

  if (payload && typeof payload === 'object' && 'code' in payload) {
    const wrapped = payload as Record<string, unknown>
    if (wrapped.code === 0) {
      return wrapped.data as T
    }
    throw normalizeGuestError(response.status, wrapped)
  }

  return payload as T
}

async function readJson(response: Response): Promise<unknown> {
  const text = await response.text()
  if (!text) return null
  try {
    return JSON.parse(text)
  } catch {
    return { message: text }
  }
}

function normalizeGuestError(status: number, payload: unknown): Record<string, unknown> {
  const data = payload && typeof payload === 'object' ? payload as Record<string, unknown> : {}
  return {
    status,
    code: data.code,
    reason: data.reason,
    error: data.error,
    message:
      typeof data.message === 'string'
        ? data.message
        : typeof data.detail === 'string'
          ? data.detail
          : '请求失败',
    metadata: data.metadata,
  }
}

function isAuthResponse(response: GuestLoginResponseData): response is GuestAuthResponse {
  return typeof response.access_token === 'string' && !!response.user
}

function persistAuthResponse(data: GuestAuthResponse): void {
  localStorage.setItem(AUTH_TOKEN_KEY, data.access_token)
  if (data.refresh_token) {
    localStorage.setItem(REFRESH_TOKEN_KEY, data.refresh_token)
  }
  if (data.expires_in) {
    localStorage.setItem(TOKEN_EXPIRES_AT_KEY, String(Date.now() + data.expires_in * 1000))
  }
  localStorage.setItem(AUTH_USER_KEY, JSON.stringify(data.user))
}

export function getPersistedGuestUser(): GuestUser | null {
  const raw = localStorage.getItem(AUTH_USER_KEY)
  if (!raw) return null
  try {
    return JSON.parse(raw) as GuestUser
  } catch {
    localStorage.removeItem(AUTH_USER_KEY)
    return null
  }
}

export function hasGuestAuthSession(): boolean {
  return Boolean(localStorage.getItem(AUTH_TOKEN_KEY) && getPersistedGuestUser())
}

export async function loginGuest(credentials: {
  email: string
  password: string
  turnstile_token?: string
}): Promise<GuestLoginResponseData> {
  const data = await guestPost<GuestLoginResponseData>('/user/login', credentials)
  if (isAuthResponse(data)) {
    persistAuthResponse(data)
  }
  return data
}

export async function registerGuest(userData: GuestRegisterRequest): Promise<GuestAuthResponse> {
  const data = await guestPost<GuestAuthResponse>('/user/register', userData)
  persistAuthResponse(data)
  return data
}

export async function validateGuestPromoCode(code: string): Promise<GuestValidatePromoCodeResponse> {
  return guestPost<GuestValidatePromoCodeResponse>('/api/v1/auth/validate-promo-code', { code })
}

export async function validateGuestInvitationCode(code: string): Promise<GuestValidateInvitationCodeResponse> {
  return guestPost<GuestValidateInvitationCodeResponse>('/api/v1/auth/validate-invitation-code', { code })
}

export function getGuestErrorMessage(error: unknown, fallback: string): string {
  if (!error || typeof error !== 'object') return fallback
  const err = error as { message?: string; error?: string; response?: { data?: { message?: string; detail?: string } } }
  return err.response?.data?.detail || err.response?.data?.message || err.message || err.error || fallback
}

export function isGuestAdditionalVerificationRequired(response: GuestLoginResponseData): boolean {
  return !isAuthResponse(response)
}

export function clearGuestAffiliateCode(): void {
  try {
    localStorage.removeItem('affiliate_referral_code')
  } catch {
    // Ignore storage failures.
  }
}

export function resolveGuestAffiliateCode(...values: unknown[]): string {
  for (const value of values) {
    const raw = Array.isArray(value) ? value[0] : value
    if (typeof raw === 'string' && raw.trim()) {
      const code = raw.trim()
      try {
        localStorage.setItem(
          'affiliate_referral_code',
          JSON.stringify({ code, expiresAt: Date.now() + 30 * 24 * 60 * 60 * 1000 }),
        )
      } catch {
        // Ignore storage failures.
      }
      return code
    }
  }

  try {
    const raw = localStorage.getItem('affiliate_referral_code')
    if (!raw) return ''
    const parsed = JSON.parse(raw) as { code?: string; expiresAt?: number }
    if (!parsed.code || !parsed.expiresAt || parsed.expiresAt <= Date.now()) {
      localStorage.removeItem('affiliate_referral_code')
      return ''
    }
    return parsed.code.trim()
  } catch {
    return ''
  }
}
