import axios, { type AxiosError, type AxiosResponse } from 'axios'
import { getLocale } from '@/i18n'
import type {
  ApiResponse,
  AuthResponse,
  CurrentUserResponse,
  LoginRequest,
  PublicSettings,
  RegisterRequest,
  TotpLogin2FARequest,
  TotpLoginResponse,
} from '@/types'
import type {
  ValidateInvitationCodeResponse,
  ValidatePromoCodeResponse,
  WeChatOAuthPublicSettings,
  WeChatOAuthUnavailableReason,
  WeChatOAuthMode,
  ResolvedWeChatOAuthStart,
} from './auth'

export type LoginResponse = AuthResponse | TotpLoginResponse

const AUTH_TOKEN_KEY = 'auth_token'
const AUTH_USER_KEY = 'auth_user'
const REFRESH_TOKEN_KEY = 'refresh_token'
const TOKEN_EXPIRES_AT_KEY = 'token_expires_at'

const publicClient = axios.create({
  baseURL: '',
  withCredentials: true,
  timeout: 30000,
  headers: {
    'Content-Type': 'application/json'
  }
})

publicClient.interceptors.request.use((config) => {
  if (config.headers) {
    config.headers['Accept-Language'] = getLocale()
  }
  return config
})

publicClient.interceptors.response.use(
  (response: AxiosResponse) => {
    const apiResponse = response.data as ApiResponse<unknown>
    if (apiResponse && typeof apiResponse === 'object' && 'code' in apiResponse) {
      if (apiResponse.code === 0) {
        response.data = apiResponse.data
      } else {
        const resp = apiResponse as unknown as Record<string, unknown>
        return Promise.reject({
          status: response.status,
          code: apiResponse.code,
          message: apiResponse.message || 'Unknown error',
          reason: resp.reason,
          metadata: resp.metadata,
        })
      }
    }
    return response
  },
  (error: AxiosError<ApiResponse<unknown>>) => {
    if (error.code === 'ERR_CANCELED' || axios.isCancel(error)) {
      return Promise.reject(error)
    }

    if (error.response) {
      const { status, data } = error.response
      const apiData = (typeof data === 'object' && data !== null ? data : {}) as Record<string, any>
      return Promise.reject({
        status,
        code: apiData.code,
        reason: apiData.reason,
        error: apiData.error,
        message: apiData.message || apiData.detail || error.message,
        metadata: apiData.metadata,
      })
    }

    return Promise.reject({
      status: 0,
      message: 'Network error. Please check your connection.'
    })
  }
)

export function isTotp2FARequired(response: LoginResponse): response is TotpLoginResponse {
  return 'requires_2fa' in response && response.requires_2fa === true
}

export function setAuthToken(token: string): void {
  localStorage.setItem(AUTH_TOKEN_KEY, token)
}

export function setRefreshToken(token: string): void {
  localStorage.setItem(REFRESH_TOKEN_KEY, token)
}

export function setTokenExpiresAt(expiresIn: number): void {
  const expiresAt = Date.now() + expiresIn * 1000
  localStorage.setItem(TOKEN_EXPIRES_AT_KEY, String(expiresAt))
}

export function getAuthToken(): string | null {
  return localStorage.getItem(AUTH_TOKEN_KEY)
}

export function getPersistedUser(): CurrentUserResponse | null {
  const raw = localStorage.getItem(AUTH_USER_KEY)
  if (!raw) return null
  try {
    return JSON.parse(raw) as CurrentUserResponse
  } catch {
    localStorage.removeItem(AUTH_USER_KEY)
    return null
  }
}

export function getPersistedUserRole(): string {
  return getPersistedUser()?.role || ''
}

export function hasAuthSession(): boolean {
  return Boolean(getAuthToken() && getPersistedUser())
}

export function isPersistedAdmin(): boolean {
  return getPersistedUserRole() === 'admin'
}

export function clearAuthToken(): void {
  localStorage.removeItem(AUTH_TOKEN_KEY)
  localStorage.removeItem(REFRESH_TOKEN_KEY)
  localStorage.removeItem(AUTH_USER_KEY)
  localStorage.removeItem(TOKEN_EXPIRES_AT_KEY)
}

function persistAuthResponse(data: AuthResponse): void {
  setAuthToken(data.access_token)
  if (data.refresh_token) {
    setRefreshToken(data.refresh_token)
  }
  if (data.expires_in) {
    setTokenExpiresAt(data.expires_in)
  }
  localStorage.setItem(AUTH_USER_KEY, JSON.stringify(data.user))
}

export async function login(credentials: LoginRequest): Promise<LoginResponse> {
  const { data } = await publicClient.post<LoginResponse>('/user/login', credentials)
  if (!isTotp2FARequired(data)) {
    persistAuthResponse(data)
  }
  return data
}

export async function login2FA(request: TotpLogin2FARequest): Promise<AuthResponse> {
  const { data } = await publicClient.post<AuthResponse>('/user/login/2fa', request)
  persistAuthResponse(data)
  return data
}

export async function register(userData: RegisterRequest): Promise<AuthResponse> {
  const { data } = await publicClient.post<AuthResponse>('/user/register', userData)
  persistAuthResponse(data)
  return data
}

export async function getPublicSettings(): Promise<PublicSettings> {
  const { data } = await publicClient.get<PublicSettings>('/api/v1/settings/public')
  return data
}

export async function validatePromoCode(code: string): Promise<ValidatePromoCodeResponse> {
  const { data } = await publicClient.post<ValidatePromoCodeResponse>(
    '/api/v1/auth/validate-promo-code',
    { code }
  )
  return data
}

export async function validateInvitationCode(code: string): Promise<ValidateInvitationCodeResponse> {
  const { data } = await publicClient.post<ValidateInvitationCodeResponse>(
    '/api/v1/auth/validate-invitation-code',
    { code }
  )
  return data
}

export interface ForgotPasswordRequest {
  email: string
  turnstile_token?: string
  tencent_captcha_ticket?: string
  tencent_captcha_randstr?: string
}

export interface ForgotPasswordResponse {
  message: string
}

export async function forgotPassword(request: ForgotPasswordRequest): Promise<ForgotPasswordResponse> {
  const { data } = await publicClient.post<ForgotPasswordResponse>(
    '/api/v1/auth/forgot-password',
    request
  )
  return data
}

export interface ResetPasswordRequest {
  email: string
  token: string
  new_password: string
}

export interface ResetPasswordResponse {
  message: string
}

export async function resetPassword(request: ResetPasswordRequest): Promise<ResetPasswordResponse> {
  const { data } = await publicClient.post<ResetPasswordResponse>(
    '/api/v1/auth/reset-password',
    request
  )
  return data
}

export function isWeChatWebOAuthEnabled(
  settings: WeChatOAuthPublicSettings | null | undefined,
): boolean {
  const legacyEnabled = settings?.wechat_oauth_enabled ?? false
  const hasExplicitCapabilities =
    typeof settings?.wechat_oauth_open_enabled === 'boolean' ||
    typeof settings?.wechat_oauth_mp_enabled === 'boolean'

  if (!hasExplicitCapabilities) {
    return legacyEnabled
  }

  return settings?.wechat_oauth_open_enabled === true || settings?.wechat_oauth_mp_enabled === true
}

export function hasExplicitWeChatOAuthCapabilities(
  settings: WeChatOAuthPublicSettings | null | undefined,
): settings is WeChatOAuthPublicSettings & {
  wechat_oauth_open_enabled: boolean
  wechat_oauth_mp_enabled: boolean
} {
  return typeof settings?.wechat_oauth_open_enabled === 'boolean'
    && typeof settings?.wechat_oauth_mp_enabled === 'boolean'
}

export function resolveWeChatOAuthStart(
  settings: WeChatOAuthPublicSettings | null | undefined,
  userAgent?: string
): ResolvedWeChatOAuthStart {
  const normalizedUserAgent = (userAgent
    ?? (typeof navigator !== 'undefined' ? navigator.userAgent : '')
    ?? '').trim()
  const isWeChatBrowser = /MicroMessenger/i.test(normalizedUserAgent)
  const legacyEnabled = settings?.wechat_oauth_enabled ?? false
  const openEnabled = typeof settings?.wechat_oauth_open_enabled === 'boolean'
    ? settings.wechat_oauth_open_enabled
    : legacyEnabled
  const mpEnabled = typeof settings?.wechat_oauth_mp_enabled === 'boolean'
    ? settings.wechat_oauth_mp_enabled
    : legacyEnabled
  const mobileEnabled = typeof settings?.wechat_oauth_mobile_enabled === 'boolean'
    ? settings.wechat_oauth_mobile_enabled
    : false

  if (isWeChatBrowser) {
    if (mpEnabled) {
      return { mode: 'mp', openEnabled, mpEnabled, mobileEnabled, isWeChatBrowser, unavailableReason: null }
    }
    if (openEnabled) {
      return { mode: null, openEnabled, mpEnabled, mobileEnabled, isWeChatBrowser, unavailableReason: 'external_browser_required' }
    }
    return { mode: null, openEnabled, mpEnabled, mobileEnabled, isWeChatBrowser, unavailableReason: 'not_configured' }
  }

  if (openEnabled) {
    return { mode: 'open', openEnabled, mpEnabled, mobileEnabled, isWeChatBrowser, unavailableReason: null }
  }
  if (mpEnabled) {
    return { mode: null, openEnabled, mpEnabled, mobileEnabled, isWeChatBrowser, unavailableReason: 'wechat_browser_required' }
  }
  return { mode: null, openEnabled, mpEnabled, mobileEnabled, isWeChatBrowser, unavailableReason: 'not_configured' }
}

export function resolveWeChatOAuthStartStrict(
  settings: WeChatOAuthPublicSettings | null | undefined,
  userAgent?: string,
): ResolvedWeChatOAuthStart {
  const normalizedUserAgent = (userAgent
    ?? (typeof navigator !== 'undefined' ? navigator.userAgent : '')
    ?? '').trim()
  const isWeChatBrowser = /MicroMessenger/i.test(normalizedUserAgent)

  if (!hasExplicitWeChatOAuthCapabilities(settings)) {
    return {
      mode: null,
      openEnabled: false,
      mpEnabled: false,
      mobileEnabled: false,
      isWeChatBrowser,
      unavailableReason: 'capability_unknown',
    }
  }

  return resolveWeChatOAuthStart(settings, normalizedUserAgent)
}

export type {
  ResolvedWeChatOAuthStart,
  WeChatOAuthMode,
  WeChatOAuthPublicSettings,
  WeChatOAuthUnavailableReason,
}
