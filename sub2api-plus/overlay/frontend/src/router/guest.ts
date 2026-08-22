import { createRouter, createWebHistory, type RouteLocationNormalized, type RouteRecordRaw } from 'vue-router'
import { useNavigationLoadingState } from '@/composables/useNavigationLoading'
import { enterFullApp, navigateToAuthenticatedApp } from '@/public/fullAppBridge'

const GUEST_SITE_NAME = '企业数据中台'

export const GUEST_PUBLIC_PATHS = [
  '/',
  '/login',
  '/register',
  '/catalog',
  '/governance',
  '/exchange',
  '/orchestration',
  '/docs',
]

export const FULL_APP_PUBLIC_PATH_PREFIXES = [
  '/email-verify',
]

export function isGuestPublicPath(path: string): boolean {
  return GUEST_PUBLIC_PATHS.includes(path)
}

export function isFullAppPublicPath(path: string): boolean {
  return FULL_APP_PUBLIC_PATH_PREFIXES.some((prefix) => path === prefix || path.startsWith(`${prefix}/`))
}

function redirectTarget(to: RouteLocationNormalized): string {
  const redirect = to.query.redirect
  return typeof redirect === 'string' && redirect.trim() ? redirect : `/${'dashboard'}`
}

function getGuestDocumentTitle(routeTitle: unknown): string {
  return typeof routeTitle === 'string' && routeTitle.trim()
    ? `${routeTitle.trim()} - ${GUEST_SITE_NAME}`
    : GUEST_SITE_NAME
}

function backendModeEnabled(): boolean {
  return window.__STATIC_APP__?.backend_mode_enabled === true
}

function hasGuestAuthSession(): boolean {
  try {
    const token = localStorage.getItem('auth_token')
    const rawUser = localStorage.getItem('auth_user')
    if (!token || !rawUser) return false
    const user = JSON.parse(rawUser)
    return Boolean(user && typeof user === 'object')
  } catch {
    return false
  }
}

const routes: RouteRecordRaw[] = [
  {
    path: '/',
    name: 'GuestHome',
    component: () => import('@/views/guest/HomeView.vue'),
    meta: {
      requiresAuth: false,
      title: '数据中台'
    }
  },
  {
    path: '/catalog',
    name: 'GuestCatalog',
    component: () => import('@/views/guest/CatalogView.vue'),
    meta: {
      requiresAuth: false,
      title: '数据目录'
    }
  },
  {
    path: '/governance',
    name: 'GuestGovernance',
    component: () => import('@/views/guest/GovernanceView.vue'),
    meta: {
      requiresAuth: false,
      title: '数据治理'
    }
  },
  {
    path: '/exchange',
    name: 'GuestExchange',
    component: () => import('@/views/guest/ExchangeView.vue'),
    meta: {
      requiresAuth: false,
      title: '交换任务'
    }
  },
  {
    path: '/orchestration',
    name: 'GuestOrchestration',
    component: () => import('@/views/guest/OrchestrationView.vue'),
    meta: {
      requiresAuth: false,
      title: '服务编排'
    }
  },
  {
    path: '/docs',
    name: 'GuestDocs',
    component: () => import('@/views/guest/DocsView.vue'),
    meta: {
      requiresAuth: false,
      title: '接入规范'
    }
  },
  {
    path: '/login',
    name: 'Login',
    component: () => import('@/views/guest/LoginView.vue'),
    meta: {
      requiresAuth: false,
      title: '数据中台登录'
    }
  },
  {
    path: '/register',
    name: 'Register',
    component: () => import('@/views/guest/RegisterView.vue'),
    meta: {
      requiresAuth: false,
      title: '账号申请'
    }
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'GuestFullAppFallback',
    component: { template: '<div />' },
    meta: {
      title: 'Loading'
    }
  },
]

const router = createRouter({
  history: createWebHistory('/'),
  routes,
  scrollBehavior(_to, _from, savedPosition) {
    if (savedPosition) {
      return savedPosition
    }
    return { top: 0 }
  }
})

const navigationLoading = useNavigationLoadingState()

router.beforeEach(async (to, _from, next) => {
  navigationLoading.startNavigation()

  document.title = getGuestDocumentTitle(to.meta.title)
  const isAuthenticated = hasGuestAuthSession()

  if (!isGuestPublicPath(to.path)) {
    await enterFullApp(to.fullPath)
    next(false)
    return
  }

  if (isAuthenticated && (to.path === '/login' || to.path === '/register')) {
    if (backendModeEnabled()) {
      next()
      return
    }

    await navigateToAuthenticatedApp(router, redirectTarget(to))
    next(false)
    return
  }

  if (backendModeEnabled() && !isAuthenticated && to.path !== '/login') {
    next('/login')
    return
  }

  next()
})

router.afterEach(() => {
  navigationLoading.endNavigation()
})

router.onError(() => {
  navigationLoading.endNavigation()
})

export default router
