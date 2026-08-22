import type { Plugin, UserConfig } from 'vite'

/**
 * fork 手动分包配置（从 vite.config.ts 抽离）。
 * 抽离目的：分包策略是 fork 与上游的持续分歧点，独立成文件后
 * 上游对 vite.config.ts 构建配置的改动不会再与分包策略冲突。
 *
 * 与上游策略差异：
 * - Pinia 单独分包（vendor-store），不放进未登录首屏的 Vue 核心包
 * - 未匹配的小型第三方库交给 Rollup 默认处理（return undefined），不合并 vendor-misc
 */
export function manualChunks(id: string): string | undefined {
  if (id.includes('node_modules')) {
    // Pinia 只在完整后台入口使用，不放进未登录首屏的 Vue 核心包。
    if (id.includes('/pinia/')) {
      return 'vendor-store'
    }

    // Vue 核心库
    if (
      id.includes('/vue/') ||
      id.includes('/vue-router/') ||
      id.includes('/@vue/')
    ) {
      return 'vendor-vue'
    }

    // UI 工具库（较大，单独分离）
    if (id.includes('/@vueuse/') || id.includes('/xlsx/')) {
      return 'vendor-ui'
    }

    // 图表库
    if (id.includes('/chart.js/') || id.includes('/vue-chartjs/')) {
      return 'vendor-chart'
    }

    // 国际化
    if (id.includes('/vue-i18n/') || id.includes('/@intlify/')) {
      return 'vendor-i18n'
    }

    // Stripe 仅在支付流程中按需加载，避免进入首页公共依赖。
    if (id.includes('/@stripe/stripe-js/')) {
      return 'vendor-stripe'
    }

    return undefined
  }

  // 应用代码：按入口点自动分包，不手动干预
  // 这样可以避免循环依赖，同时保持合理的 chunk 数量
  return undefined
}

export function forkPublicSettingsPlugin(): Plugin {
  return {
    name: 'fork-public-settings-compat',
    apply: 'serve',
    transformIndexHtml: {
      order: 'post',
      handler(html) {
        return html
          .replace('window.__APP_CONFIG__=', 'window.__STATIC_APP__=')
          .replace(/ - AI API Gateway<\/title>/i, ' - Secure Portal</title>')
      }
    }
  }
}

export function createForkViteConfig(backendUrl: string): UserConfig {
  return {
    base: '/static/app/',
    plugins: [forkPublicSettingsPlugin()],
    build: {
      rollupOptions: {
        output: {
          entryFileNames: 'res/[hash].js',
          chunkFileNames: 'res/[hash].js',
          assetFileNames: 'res/[hash][extname]',
          manualChunks
        }
      }
    },
    server: {
      proxy: {
        '/user': {
          target: backendUrl,
          changeOrigin: true
        }
      }
    }
  }
}
