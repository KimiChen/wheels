const BACKEND_MODE_ALLOWED_PATHS = [
  '/login',
  '/key-usage',
  '/setup',
  '/payment/result',
  '/payment/airwallex',
  '/legal',
]

const BACKEND_MODE_CALLBACK_PATHS = [
  '/auth/callback',
  '/auth/linuxdo/callback',
  '/auth/dingtalk/callback',
  '/auth/dingtalk/email-completion',
  '/auth/oidc/callback',
  '/auth/wechat/callback',
  '/auth/wechat/payment/callback',
]

const BACKEND_MODE_PENDING_AUTH_PATHS = ['/register', '/email-verify']

export function isBackendModePublicRouteAllowed(
  path: string,
  hasPendingAuthSession: boolean,
): boolean {
  if (
    BACKEND_MODE_ALLOWED_PATHS.some(
      (allowedPath) => path === allowedPath || path.startsWith(allowedPath),
    )
  ) {
    return true
  }

  if (BACKEND_MODE_CALLBACK_PATHS.some((callbackPath) => path === callbackPath)) {
    return true
  }

  if (
    hasPendingAuthSession &&
    BACKEND_MODE_PENDING_AUTH_PATHS.some((allowedPath) => path === allowedPath)
  ) {
    return true
  }

  return false
}
