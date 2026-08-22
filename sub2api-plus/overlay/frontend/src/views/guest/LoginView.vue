<template>
  <GuestAuthLayout>
    <div class="space-y-6">
      <div>
        <p class="text-xs font-semibold uppercase tracking-wide text-wiki-muted">DATA FABRIC</p>
        <h2 class="mt-2 font-heading text-2xl font-semibold text-wiki-txt">数据中台登录</h2>
        <p class="mt-2 text-sm text-wiki-muted">
          使用组织账号访问数据目录、逻辑数据仓库与数据服务管理台。
        </p>
      </div>

      <div
        v-if="errorMessage"
        class="rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-600"
      >
        {{ errorMessage }}
      </div>

      <form @submit.prevent="handleLogin" class="space-y-5">
        <div>
          <label for="email" class="mb-1.5 block text-sm font-semibold text-slate-700">
            邮箱
          </label>
          <div class="relative">
            <div class="pointer-events-none absolute inset-y-0 left-0 flex items-center pl-3.5">
              <Icon name="mail" size="md" class="text-slate-400" />
            </div>
            <input
              id="email"
              v-model="formData.email"
              type="email"
              required
              autofocus
              autocomplete="email"
              :disabled="authActionDisabled"
              class="guest-input pl-11"
              :class="{ 'guest-input-error': errors.email }"
              placeholder="name@example.com"
            />
          </div>
        </div>

        <div>
          <label for="password" class="mb-1.5 block text-sm font-semibold text-slate-700">
            密码
          </label>
          <div class="relative">
            <div class="pointer-events-none absolute inset-y-0 left-0 flex items-center pl-3.5">
              <Icon name="lock" size="md" class="text-slate-400" />
            </div>
            <input
              id="password"
              v-model="formData.password"
              :type="showPassword ? 'text' : 'password'"
              required
              autocomplete="current-password"
              :disabled="authActionDisabled"
              class="guest-input pl-11 pr-11"
              :class="{ 'guest-input-error': errors.password }"
              placeholder="请输入密码"
            />
            <button
              type="button"
              @click="showPassword = !showPassword"
              :disabled="authActionDisabled"
              class="absolute inset-y-0 right-0 flex items-center pr-3.5 text-slate-400 transition-colors hover:text-slate-600 disabled:cursor-not-allowed disabled:opacity-60"
            >
              <Icon v-if="showPassword" name="eyeOff" size="md" />
              <Icon v-else name="eye" size="md" />
            </button>
          </div>
          <div class="mt-2 flex items-center justify-between">
            <span class="text-xs text-wiki-muted">统一身份认证</span>
          </div>
        </div>

        <div v-if="settings.turnstile_enabled && settings.turnstile_site_key">
          <TurnstileWidget
            ref="turnstileRef"
            :site-key="settings.turnstile_site_key"
            @verify="onTurnstileVerify"
            @expire="onTurnstileExpire"
            @error="onTurnstileError"
          />
        </div>

        <button
          type="submit"
          :disabled="authActionDisabled || (settings.turnstile_enabled && !turnstileToken)"
          class="inline-flex w-full items-center justify-center gap-2 rounded-xl bg-wiki-accent px-4 py-3 text-sm font-semibold text-white shadow-md shadow-indigo-200 transition-all hover:-translate-y-0.5 hover:bg-indigo-600 hover:shadow-lg hover:shadow-indigo-200 disabled:translate-y-0 disabled:cursor-not-allowed disabled:opacity-60"
        >
          <svg
            v-if="isLoading"
            class="h-4 w-4 animate-spin text-white"
            fill="none"
            viewBox="0 0 24 24"
          >
            <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
            <path
              class="opacity-75"
              fill="currentColor"
              d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"
            />
          </svg>
          <Icon v-else name="login" size="sm" :stroke-width="2" />
          <span>{{ isLoading ? '登录中...' : '登录' }}</span>
        </button>

        <LoginAgreementPrompt
          v-if="loginAgreementEnabled"
          :accepted="agreementAccepted"
          :documents="settings.login_agreement_documents"
          :mode="settings.login_agreement_mode"
          :updated-at="settings.login_agreement_updated_at"
          :visible="showAgreementModal"
          @accept="acceptLoginAgreement"
          @reject="rejectLoginAgreement"
          @open="showAgreementModal = true"
        />
      </form>
    </div>

    <template v-if="!settings.backend_mode_enabled" #footer>
      <p class="text-wiki-muted">
        还没有账号？
        <router-link
          to="/register"
          class="font-semibold text-wiki-accent transition-colors hover:text-indigo-500"
        >
          注册
        </router-link>
      </p>
    </template>
  </GuestAuthLayout>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import GuestAuthLayout from './GuestAuthLayout.vue'
import LoginAgreementPrompt from '@/components/auth/LoginAgreementPrompt.vue'
import Icon from '@/components/icons/Icon.vue'
import TurnstileWidget from '@/components/TurnstileWidget.vue'
import {
  clearGuestAffiliateCode,
  getGuestErrorMessage,
  isGuestAdditionalVerificationRequired,
  loginGuest,
} from '@/api/guestAuth'
import { navigateToAuthenticatedApp } from '@/public/fullAppBridge'
import { useGuestToast } from '@/composables/useGuestToast'

const LOGIN_AGREEMENT_STORAGE_KEY = 'portal_login_agreement_consent'

interface GuestLoginAgreementDocument {
  id: string
  title: string
  content_md: string
}

interface GuestLoginSettings {
  turnstile_enabled: boolean
  turnstile_site_key: string
  login_agreement_enabled: boolean
  login_agreement_mode: 'modal' | 'checkbox'
  login_agreement_updated_at: string
  login_agreement_revision: string
  login_agreement_documents: GuestLoginAgreementDocument[]
  backend_mode_enabled: boolean
}

function readStaticApp(): Record<string, unknown> {
  const payload = window.__STATIC_APP__
  return payload && typeof payload === 'object' ? payload as Record<string, unknown> : {}
}

function stringValue(value: unknown): string {
  return typeof value === 'string' && value.trim() ? value.trim() : ''
}

function documentArrayValue(value: unknown): GuestLoginAgreementDocument[] {
  if (!Array.isArray(value)) return []
  return value
    .filter((item): item is Record<string, unknown> => item !== null && typeof item === 'object')
    .map((item) => ({
      id: stringValue(item.id) || stringValue(item.title),
      title: stringValue(item.title),
      content_md: stringValue(item.content_md),
    }))
    .filter((item) => item.title)
}

function getGuestLoginSettings(): GuestLoginSettings {
  const payload = readStaticApp()
  const documents = documentArrayValue(payload.login_agreement_documents)
  return {
    turnstile_enabled: payload.turnstile_enabled === true,
    turnstile_site_key: stringValue(payload.turnstile_site_key),
    login_agreement_enabled: payload.login_agreement_enabled === true && documents.length > 0,
    login_agreement_mode: payload.login_agreement_mode === 'checkbox' ? 'checkbox' : 'modal',
    login_agreement_updated_at: stringValue(payload.login_agreement_updated_at),
    login_agreement_revision: stringValue(payload.login_agreement_revision),
    login_agreement_documents: documents,
    backend_mode_enabled: payload.backend_mode_enabled === true,
  }
}

const router = useRouter()
const toast = useGuestToast()
const settings = getGuestLoginSettings()

const isLoading = ref(false)
const errorMessage = ref('')
const showPassword = ref(false)
const agreementAccepted = ref(false)
const showAgreementModal = ref(false)
const turnstileRef = ref<InstanceType<typeof TurnstileWidget> | null>(null)
const turnstileToken = ref('')

const formData = reactive({
  email: '',
  password: '',
})

const errors = reactive({
  email: '',
  password: '',
  turnstile: '',
})

const loginAgreementEnabled = computed(
  () => settings.login_agreement_enabled && settings.login_agreement_documents.length > 0,
)
const agreementRevision = computed(
  () =>
    settings.login_agreement_revision ||
    `${settings.login_agreement_updated_at}:${settings.login_agreement_documents
      .map((doc) => `${doc.id}:${doc.title}`)
      .join('|')}`,
)
const agreementGateActive = computed(() => loginAgreementEnabled.value && !agreementAccepted.value)
const authActionDisabled = computed(() => isLoading.value || agreementGateActive.value)
const validationToastMessage = computed(() => errors.email || errors.password || errors.turnstile || '')

watch(validationToastMessage, (value, previousValue) => {
  if (value && value !== previousValue) {
    toast.showError(value)
  }
})

agreementAccepted.value = !loginAgreementEnabled.value || hasAcceptedLoginAgreement(agreementRevision.value)
showAgreementModal.value =
  loginAgreementEnabled.value && !agreementAccepted.value && settings.login_agreement_mode !== 'checkbox'

const expiredFlag = sessionStorage.getItem('auth_expired')
if (expiredFlag) {
  sessionStorage.removeItem('auth_expired')
  errorMessage.value = '登录状态已过期，请重新登录'
  toast.showWarning(errorMessage.value)
}

function hasAcceptedLoginAgreement(revision: string): boolean {
  if (!revision) return false
  try {
    const raw = localStorage.getItem(LOGIN_AGREEMENT_STORAGE_KEY)
    if (!raw) return false
    const parsed = JSON.parse(raw) as { revision?: string }
    return parsed.revision === revision
  } catch {
    return false
  }
}

function acceptLoginAgreement(): void {
  if (agreementRevision.value) {
    localStorage.setItem(
      LOGIN_AGREEMENT_STORAGE_KEY,
      JSON.stringify({ revision: agreementRevision.value, accepted_at: new Date().toISOString() }),
    )
  }
  agreementAccepted.value = true
  showAgreementModal.value = false
}

function rejectLoginAgreement(): void {
  localStorage.removeItem(LOGIN_AGREEMENT_STORAGE_KEY)
  agreementAccepted.value = false
  showAgreementModal.value = false
  toast.showWarning('未同意最新条款前，无法输入账号密码。')
}

function onTurnstileVerify(token: string): void {
  turnstileToken.value = token
  errors.turnstile = ''
}

function onTurnstileExpire(): void {
  turnstileToken.value = ''
  errors.turnstile = '验证已过期，请重新验证'
}

function onTurnstileError(): void {
  turnstileToken.value = ''
  errors.turnstile = '验证失败，请重试'
}

function validateForm(): boolean {
  errors.email = ''
  errors.password = ''
  errors.turnstile = ''

  let valid = true
  if (agreementGateActive.value) {
    toast.showWarning('请先阅读并同意最新条款后再登录。')
    if (settings.login_agreement_mode !== 'checkbox') {
      showAgreementModal.value = true
    }
    return false
  }
  if (!formData.email.trim()) {
    errors.email = '请输入邮箱'
    valid = false
  } else if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(formData.email)) {
    errors.email = '请输入有效邮箱地址'
    valid = false
  }
  if (!formData.password) {
    errors.password = '请输入密码'
    valid = false
  } else if (formData.password.length < 6) {
    errors.password = '密码至少 6 位'
    valid = false
  }
  if (settings.turnstile_enabled && !turnstileToken.value) {
    errors.turnstile = '请先完成人机验证'
    valid = false
  }
  return valid
}

async function handleLogin(): Promise<void> {
  errorMessage.value = ''
  if (!validateForm()) return

  isLoading.value = true
  try {
    const response = await loginGuest({
      email: formData.email,
      password: formData.password,
      turnstile_token: settings.turnstile_enabled ? turnstileToken.value : undefined,
    })

    if (isGuestAdditionalVerificationRequired(response)) {
      errorMessage.value = '当前账号需要二次验证，请进入完整登录页完成验证。'
      toast.showWarning(errorMessage.value)
      return
    }

    clearGuestAffiliateCode()
    toast.showSuccess('登录成功')
    const redirectTo = (router.currentRoute.value.query.redirect as string) || `/${'dashboard'}`
    await navigateToAuthenticatedApp(router, redirectTo)
  } catch (error) {
    turnstileRef.value?.reset()
    turnstileToken.value = ''
    errorMessage.value = getGuestErrorMessage(error, '登录失败')
    toast.showError(errorMessage.value)
  } finally {
    isLoading.value = false
  }
}
</script>

<style scoped>
.guest-input {
  width: 100%;
  border-radius: 0.75rem;
  border: 1px solid #e2e8f0;
  background: #fff;
  padding-top: 0.75rem;
  padding-bottom: 0.75rem;
  color: #0f172a;
  font-size: 0.875rem;
  outline: none;
  transition:
    border-color 0.2s ease,
    box-shadow 0.2s ease;
}

.guest-input::placeholder {
  color: #94a3b8;
}

.guest-input:focus {
  border-color: #6366f1;
  box-shadow: 0 0 0 3px rgba(99, 102, 241, 0.1);
}

.guest-input:disabled {
  cursor: not-allowed;
  background: #f8fafc;
  color: #94a3b8;
}

.guest-input-error,
.guest-input-error:focus {
  border-color: #ef4444;
  box-shadow: 0 0 0 3px rgba(239, 68, 68, 0.12);
}
</style>
