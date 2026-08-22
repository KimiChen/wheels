import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
  enterFullApp: vi.fn(),
  isReady: vi.fn(),
  mount: vi.fn(),
  setPublicApp: vi.fn(),
}))

vi.mock('vue', () => ({
  createApp: vi.fn(() => {
    const app = {
      mount: mocks.mount,
      use: vi.fn(() => app),
    }
    return app
  }),
}))

vi.mock('./PublicApp.vue', () => ({
  default: {},
}))

vi.mock('./router/guest', () => ({
  default: {
    isReady: mocks.isReady,
  },
  isFullAppPublicPath: (path: string) => path === '/email-verify' || path.startsWith('/email-verify/'),
  isGuestPublicPath: (path: string) => ['/', '/login', '/register'].includes(path),
}))

vi.mock('@/public/fullAppBridge', () => ({
  enterFullApp: mocks.enterFullApp,
  setPublicApp: mocks.setPublicApp,
}))

describe('index-main bootstrap auth routing', () => {
  beforeEach(() => {
    vi.resetModules()
    vi.clearAllMocks()
    localStorage.clear()
    localStorage.setItem('theme', 'light')
    mocks.isReady.mockResolvedValue(undefined)
    window.history.replaceState(null, '', '/')
  })

  it('redirects a protected startup URL to login when only a stale token remains', async () => {
    localStorage.setItem('auth_token', 'stale-token')
    window.history.replaceState(null, '', '/admin/accounts')

    await import('./index-main')

    await vi.waitFor(() => expect(mocks.mount).toHaveBeenCalledWith('#app'))
    expect(window.location.pathname).toBe('/login')
    expect(new URLSearchParams(window.location.search).get('redirect')).toBe('/admin/accounts')
    expect(mocks.enterFullApp).not.toHaveBeenCalled()
  })

  it('enters the full app for protected startup URLs with a complete session', async () => {
    localStorage.setItem('auth_token', 'valid-token')
    localStorage.setItem('auth_user', JSON.stringify({ id: 1, role: 'admin' }))
    window.history.replaceState(null, '', '/admin/accounts')

    await import('./index-main')

    await vi.waitFor(() => expect(mocks.enterFullApp).toHaveBeenCalledWith('/admin/accounts'))
    expect(mocks.mount).not.toHaveBeenCalled()
  })
})
