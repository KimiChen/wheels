import type { App as VueApp } from 'vue'
import { createApp } from 'vue'
import PublicApp from './PublicApp.vue'
import publicRouter, {
  isFullAppPublicPath,
  isGuestPublicPath,
} from './router/guest'
import { enterFullApp, setPublicApp } from '@/public/fullAppBridge'
import { isIOSDevice } from '@/utils/device'
import './style.css'
import './style-fork.css'

const GUEST_SITE_NAME = '企业数据中台'

function initIOSViewportZoomFix() {
  // iOS Safari 会在聚焦字号小于 16px 的输入框时自动放大页面，失焦后也不会自动恢复。
  // 仅在 iOS 设备追加 maximum-scale，避免改变 Android Chrome 等浏览器的手动缩放行为。
  // 此入口同时负责访客门户和完整应用，因此在路由分流前执行一次即可覆盖两种界面。
  if (!isIOSDevice()) return

  const viewport = document.querySelector('meta[name="viewport"]')
  if (!viewport) return

  const content = viewport.getAttribute('content') || ''
  if (/maximum-scale/i.test(content)) return
  viewport.setAttribute('content', `${content}, maximum-scale=1.0`)
}

function initThemeClass() {
  const savedTheme = localStorage.getItem('theme')
  const shouldUseDark =
    savedTheme === 'dark' ||
    (!savedTheme && window.matchMedia('(prefers-color-scheme: dark)').matches)
  document.documentElement.classList.toggle('dark', shouldUseDark)
}

function currentPath(): string {
  return `${window.location.pathname}${window.location.search}${window.location.hash}`
}

function hasPersistedAuthSession(): boolean {
  try {
    const token = localStorage.getItem('auth_token')
    const rawUser = localStorage.getItem('auth_user')
    if (!token || !rawUser) {
      return false
    }
    const user = JSON.parse(rawUser)
    return Boolean(user && typeof user === 'object')
  } catch {
    return false
  }
}

function redirectToLoginWithReturnPath(): void {
  const redirect = currentPath()
  const url = new URL('/login', window.location.origin)
  if (redirect && redirect !== '/login') {
    url.searchParams.set('redirect', redirect)
  }
  window.history.replaceState(null, '', `${url.pathname}${url.search}${url.hash}`)
}

async function mountPublicApp(): Promise<VueApp<Element>> {
  initThemeClass()

  const app = createApp(PublicApp)

  document.title = `${GUEST_SITE_NAME} - Secure Portal`

  app.use(publicRouter)
  setPublicApp(app)

  await publicRouter.isReady()
  app.mount('#app')
  return app
}

async function bootstrap() {
  initIOSViewportZoomFix()

  const pathname = window.location.pathname

  if (!isGuestPublicPath(pathname)) {
    if (hasPersistedAuthSession() || isFullAppPublicPath(pathname)) {
      await enterFullApp(currentPath())
      return
    }

    redirectToLoginWithReturnPath()
  }

  await mountPublicApp()
}

bootstrap()
