<template>
  <GuestAuthLayout>
    <div class="space-y-6">
      <div>
        <p class="text-xs font-semibold uppercase tracking-wide text-wiki-muted">DATA FABRIC</p>
        <h2 class="mt-2 font-heading text-2xl font-semibold text-wiki-txt">
          申请数据中台账号
        </h2>
        <p class="mt-2 text-sm text-wiki-muted">
          提交组织邮箱后，可按权限访问数据目录、数据治理与服务编排能力。
        </p>
      </div>

      <div
        v-if="errorMessage"
        class="rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-600"
      >
        {{ errorMessage }}
      </div>

      <div
        v-if="!settings.registration_enabled"
        class="rounded-xl border border-amber-200 bg-amber-50 p-4"
      >
        <div class="flex items-start gap-3">
          <Icon name="exclamationCircle" size="md" class="flex-shrink-0 text-amber-500" />
          <p class="text-sm text-amber-700">当前暂未开放注册，请联系管理员开通账号。</p>
        </div>
      </div>

      <form v-else @submit.prevent="handleRegister" class="space-y-5">
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
              :disabled="registrationActionDisabled"
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
              autocomplete="new-password"
              :disabled="registrationActionDisabled"
              class="guest-input pl-11 pr-11"
              :class="{ 'guest-input-error': errors.password }"
              placeholder="创建登录密码"
            />
            <button
              type="button"
              :disabled="registrationActionDisabled"
              @click="showPassword = !showPassword"
              class="absolute inset-y-0 right-0 flex items-center pr-3.5 text-slate-400 transition-colors hover:text-slate-600 disabled:cursor-not-allowed disabled:opacity-60"
            >
              <Icon v-if="showPassword" name="eyeOff" size="md" />
              <Icon v-else name="eye" size="md" />
            </button>
          </div>
          <p class="mt-2 text-xs text-wiki-muted">至少 6 位字符</p>
        </div>

        <div v-if="settings.invitation_code_enabled">
          <label for="invitation_code" class="mb-1.5 block text-sm font-semibold text-slate-700">
            邀请码
          </label>
          <div class="relative">
            <div class="pointer-events-none absolute inset-y-0 left-0 flex items-center pl-3.5">
              <Icon name="key" size="md" :class="invitationValidation.valid ? 'text-green-500' : 'text-slate-400'" />
            </div>
            <input
              id="invitation_code"
              v-model="formData.invitation_code"
              type="text"
              :disabled="registrationActionDisabled"
              class="guest-input pl-11 pr-10"
              :class="{
                'guest-input-valid': invitationValidation.valid,
                'guest-input-error': invitationValidation.invalid || errors.invitation_code,
              }"
              placeholder="请输入邀请码"
              @input="handleInvitationCodeInput"
            />
            <div v-if="invitationValidating" class="absolute inset-y-0 right-0 flex items-center pr-3.5">
              <span class="h-4 w-4 animate-spin rounded-full border-2 border-slate-300 border-t-slate-500"></span>
            </div>
            <div v-else-if="invitationValidation.valid" class="absolute inset-y-0 right-0 flex items-center pr-3.5">
              <Icon name="checkCircle" size="md" class="text-green-500" />
            </div>
            <div v-else-if="invitationValidation.invalid || errors.invitation_code" class="absolute inset-y-0 right-0 flex items-center pr-3.5">
              <Icon name="exclamationCircle" size="md" class="text-red-500" />
            </div>
          </div>
          <transition name="fade">
            <div v-if="invitationValidation.valid" class="mt-2 flex items-center gap-2 rounded-lg bg-green-50 px-3 py-2">
              <Icon name="checkCircle" size="sm" class="text-green-600" />
              <span class="text-sm text-green-700">邀请码有效</span>
            </div>
          </transition>
        </div>

        <div v-if="settings.promo_code_enabled">
          <label for="promo_code" class="mb-1.5 block text-sm font-semibold text-slate-700">
            优惠码
            <span class="ml-1 text-xs font-normal text-slate-400">可选</span>
          </label>
          <div class="relative">
            <div class="pointer-events-none absolute inset-y-0 left-0 flex items-center pl-3.5">
              <Icon name="gift" size="md" :class="promoValidation.valid ? 'text-green-500' : 'text-slate-400'" />
            </div>
            <input
              id="promo_code"
              v-model="formData.promo_code"
              type="text"
              :disabled="registrationActionDisabled"
              class="guest-input pl-11 pr-10"
              :class="{
                'guest-input-valid': promoValidation.valid,
                'guest-input-error': promoValidation.invalid,
              }"
              placeholder="请输入优惠码"
              @input="handlePromoCodeInput"
            />
            <div v-if="promoValidating" class="absolute inset-y-0 right-0 flex items-center pr-3.5">
              <span class="h-4 w-4 animate-spin rounded-full border-2 border-slate-300 border-t-slate-500"></span>
            </div>
            <div v-else-if="promoValidation.valid" class="absolute inset-y-0 right-0 flex items-center pr-3.5">
              <Icon name="checkCircle" size="md" class="text-green-500" />
            </div>
            <div v-else-if="promoValidation.invalid" class="absolute inset-y-0 right-0 flex items-center pr-3.5">
              <Icon name="exclamationCircle" size="md" class="text-red-500" />
            </div>
          </div>
          <transition name="fade">
            <div v-if="promoValidation.valid" class="mt-2 flex items-center gap-2 rounded-lg bg-green-50 px-3 py-2">
              <Icon name="gift" size="sm" class="text-green-600" />
              <span class="text-sm text-green-700">
                优惠码有效，奖励 {{ promoValidation.bonusAmount?.toFixed(2) }}
              </span>
            </div>
          </transition>
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

        <button
          type="submit"
          :disabled="registrationActionDisabled || (settings.turnstile_enabled && !turnstileToken)"
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
          <Icon v-else name="userPlus" size="sm" :stroke-width="2" />
          <span>{{ isLoading ? '处理中...' : settings.email_verify_enabled ? '继续' : '创建账号' }}</span>
        </button>
      </form>
    </div>

    <template #footer>
      <p class="text-wiki-muted">
        已有账号？
        <router-link
          to="/login"
          class="font-semibold text-wiki-accent transition-colors hover:text-indigo-500"
        >
          登录
        </router-link>
      </p>
    </template>
  </GuestAuthLayout>
</template>

<script setup lang="ts">
import { computed, onUnmounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import GuestAuthLayout from './GuestAuthLayout.vue'
import LoginAgreementPrompt from '@/components/auth/LoginAgreementPrompt.vue'
import Icon from '@/components/icons/Icon.vue'
import TurnstileWidget from '@/components/TurnstileWidget.vue'
import {
  clearGuestAffiliateCode,
  getGuestErrorMessage,
  registerGuest,
  resolveGuestAffiliateCode,
  validateGuestInvitationCode,
  validateGuestPromoCode,
} from '@/api/guestAuth'
import { navigateToAuthenticatedApp } from '@/public/fullAppBridge'
import {
  formatRegistrationEmailSuffixWhitelistForMessage,
  isRegistrationEmailSuffixAllowed,
  normalizeRegistrationEmailSuffixWhitelist,
} from '@/utils/registrationEmailPolicy'
import { useGuestToast } from '@/composables/useGuestToast'

const LOGIN_AGREEMENT_STORAGE_KEY = 'portal_login_agreement_consent'
const GUEST_SITE_NAME = '企业数据中台'

interface GuestLoginAgreementDocument {
  id: string
  title: string
  content_md: string
}

interface GuestRegisterSettings {
  registration_enabled: boolean
  email_verify_enabled: boolean
  registration_email_suffix_whitelist: string[]
  promo_code_enabled: boolean
  invitation_code_enabled: boolean
  turnstile_enabled: boolean
  turnstile_site_key: string
  login_agreement_enabled: boolean
  login_agreement_mode: 'modal' | 'checkbox'
  login_agreement_updated_at: string
  login_agreement_revision: string
  login_agreement_documents: GuestLoginAgreementDocument[]
}

function readStaticApp(): Record<string, unknown> {
  const payload = window.__STATIC_APP__
  return payload && typeof payload === 'object' ? payload as Record<string, unknown> : {}
}

function stringValue(value: unknown): string {
  return typeof value === 'string' && value.trim() ? value.trim() : ''
}

function stringArrayValue(value: unknown): string[] {
  if (!Array.isArray(value)) return []
  return value.filter((item): item is string => typeof item === 'string' && item.trim() !== '')
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

function getGuestRegisterSettings(): GuestRegisterSettings {
  const payload = readStaticApp()
  const documents = documentArrayValue(payload.login_agreement_documents)
  return {
    registration_enabled: payload.registration_enabled === true,
    email_verify_enabled: payload.email_verify_enabled === true,
    registration_email_suffix_whitelist: stringArrayValue(payload.registration_email_suffix_whitelist),
    promo_code_enabled: payload.promo_code_enabled === true,
    invitation_code_enabled: payload.invitation_code_enabled === true,
    turnstile_enabled: payload.turnstile_enabled === true,
    turnstile_site_key: stringValue(payload.turnstile_site_key),
    login_agreement_enabled: payload.login_agreement_enabled === true && documents.length > 0,
    login_agreement_mode: payload.login_agreement_mode === 'checkbox' ? 'checkbox' : 'modal',
    login_agreement_updated_at: stringValue(payload.login_agreement_updated_at),
    login_agreement_revision: stringValue(payload.login_agreement_revision),
    login_agreement_documents: documents,
  }
}

const router = useRouter()
const route = useRoute()
const toast = useGuestToast()
const settings = getGuestRegisterSettings()

const isLoading = ref(false)
const errorMessage = ref('')
const showPassword = ref(false)
const turnstileRef = ref<InstanceType<typeof TurnstileWidget> | null>(null)
const turnstileToken = ref('')
const agreementAccepted = ref(false)
const showAgreementModal = ref(false)
const promoValidating = ref(false)
const invitationValidating = ref(false)
const registrationEmailSuffixWhitelist = normalizeRegistrationEmailSuffixWhitelist(
  settings.registration_email_suffix_whitelist,
)

let promoValidateTimeout: ReturnType<typeof setTimeout> | null = null
let invitationValidateTimeout: ReturnType<typeof setTimeout> | null = null

const formData = reactive({
  email: '',
  password: '',
  promo_code: '',
  invitation_code: '',
  aff_code: '',
})

const errors = reactive({
  email: '',
  password: '',
  turnstile: '',
  invitation_code: '',
})

const promoValidation = reactive({
  valid: false,
  invalid: false,
  bonusAmount: null as number | null,
  message: '',
})

const invitationValidation = reactive({
  valid: false,
  invalid: false,
  message: '',
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
const registrationActionDisabled = computed(() => isLoading.value || agreementGateActive.value)
const validationToastMessage = computed(
  () =>
    errors.email ||
    errors.password ||
    (invitationValidation.invalid ? invitationValidation.message : '') ||
    errors.invitation_code ||
    (promoValidation.invalid ? promoValidation.message : '') ||
    errors.turnstile ||
    '',
)

watch(validationToastMessage, (value, previousValue) => {
  if (value && value !== previousValue) {
    toast.showError(value)
  }
})

agreementAccepted.value = !loginAgreementEnabled.value || hasAcceptedLoginAgreement(agreementRevision.value)
showAgreementModal.value =
  loginAgreementEnabled.value && !agreementAccepted.value && settings.login_agreement_mode !== 'checkbox'
formData.aff_code = resolveGuestAffiliateCode(route.query.aff, route.query.aff_code)

if (settings.promo_code_enabled && typeof route.query.promo === 'string' && route.query.promo.trim()) {
  formData.promo_code = route.query.promo.trim()
  void validatePromoCodeDebounced(formData.promo_code)
}

watch(
  () => [route.query.aff, route.query.aff_code],
  () => {
    formData.aff_code = resolveGuestAffiliateCode(route.query.aff, route.query.aff_code)
  },
)

onUnmounted(() => {
  if (promoValidateTimeout) clearTimeout(promoValidateTimeout)
  if (invitationValidateTimeout) clearTimeout(invitationValidateTimeout)
})

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
  toast.showWarning('未同意最新条款前，无法注册。')
}

function handlePromoCodeInput(): void {
  const code = formData.promo_code.trim()
  promoValidation.valid = false
  promoValidation.invalid = false
  promoValidation.bonusAmount = null
  promoValidation.message = ''
  if (!code) {
    promoValidating.value = false
    return
  }
  if (promoValidateTimeout) clearTimeout(promoValidateTimeout)
  promoValidateTimeout = setTimeout(() => {
    void validatePromoCodeDebounced(code)
  }, 500)
}

async function validatePromoCodeDebounced(code: string): Promise<void> {
  if (!code.trim()) return
  promoValidating.value = true
  try {
    const result = await validateGuestPromoCode(code)
    if (result.valid) {
      promoValidation.valid = true
      promoValidation.invalid = false
      promoValidation.bonusAmount = result.bonus_amount || 0
      promoValidation.message = ''
    } else {
      promoValidation.valid = false
      promoValidation.invalid = true
      promoValidation.bonusAmount = null
      promoValidation.message = getPromoErrorMessage(result.error_code)
    }
  } catch {
    promoValidation.valid = false
    promoValidation.invalid = true
    promoValidation.message = '优惠码无效'
  } finally {
    promoValidating.value = false
  }
}

function getPromoErrorMessage(errorCode?: string): string {
  if (errorCode === 'PROMO_CODE_NOT_FOUND') return '优惠码不存在'
  if (errorCode === 'PROMO_CODE_EXPIRED') return '优惠码已过期'
  if (errorCode === 'PROMO_CODE_DISABLED') return '优惠码已停用'
  if (errorCode === 'PROMO_CODE_MAX_USED') return '优惠码已达使用上限'
  if (errorCode === 'PROMO_CODE_ALREADY_USED') return '优惠码已使用'
  return '优惠码无效'
}

function handleInvitationCodeInput(): void {
  const code = formData.invitation_code.trim()
  invitationValidation.valid = false
  invitationValidation.invalid = false
  invitationValidation.message = ''
  errors.invitation_code = ''
  if (!code) return
  if (invitationValidateTimeout) clearTimeout(invitationValidateTimeout)
  invitationValidateTimeout = setTimeout(() => {
    void validateInvitationCodeDebounced(code)
  }, 500)
}

async function validateInvitationCodeDebounced(code: string): Promise<void> {
  invitationValidating.value = true
  try {
    const result = await validateGuestInvitationCode(code)
    invitationValidation.valid = result.valid
    invitationValidation.invalid = !result.valid
    invitationValidation.message = result.valid ? '' : '邀请码无效'
  } catch {
    invitationValidation.valid = false
    invitationValidation.invalid = true
    invitationValidation.message = '邀请码无效'
  } finally {
    invitationValidating.value = false
  }
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

function buildEmailSuffixNotAllowedMessage(): string {
  if (registrationEmailSuffixWhitelist.length === 0) return '该邮箱后缀暂不允许注册'
  return `该邮箱后缀暂不允许注册，请使用 ${formatRegistrationEmailSuffixWhitelistForMessage(
    registrationEmailSuffixWhitelist,
    { separator: '、', more: (count) => `等 ${count} 个后缀` },
  )}`
}

function validateForm(): boolean {
  errors.email = ''
  errors.password = ''
  errors.turnstile = ''
  errors.invitation_code = ''

  let valid = true
  if (agreementGateActive.value) {
    toast.showWarning('请先阅读并同意最新条款后再注册。')
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
  } else if (!isRegistrationEmailSuffixAllowed(formData.email, registrationEmailSuffixWhitelist)) {
    errors.email = buildEmailSuffixNotAllowedMessage()
    valid = false
  }
  if (!formData.password) {
    errors.password = '请输入密码'
    valid = false
  } else if (formData.password.length < 6) {
    errors.password = '密码至少 6 位'
    valid = false
  }
  if (settings.invitation_code_enabled && !formData.invitation_code.trim()) {
    errors.invitation_code = '请输入邀请码'
    valid = false
  }
  if (settings.turnstile_enabled && !turnstileToken.value) {
    errors.turnstile = '请先完成人机验证'
    valid = false
  }
  return valid
}

async function handleRegister(): Promise<void> {
  errorMessage.value = ''
  if (!validateForm()) return

  if (formData.promo_code.trim()) {
    if (promoValidating.value) {
      errorMessage.value = '优惠码校验中，请稍候'
      return
    }
    if (promoValidation.invalid) {
      errorMessage.value = '优惠码无效，无法注册'
      return
    }
  }

  if (settings.invitation_code_enabled) {
    if (invitationValidating.value) {
      errorMessage.value = '邀请码校验中，请稍候'
      return
    }
    if (invitationValidation.invalid) {
      errorMessage.value = '邀请码无效，无法注册'
      return
    }
    if (formData.invitation_code.trim() && !invitationValidation.valid) {
      errorMessage.value = '邀请码校验中，请稍候'
      await validateInvitationCodeDebounced(formData.invitation_code.trim())
      if (!invitationValidation.valid) {
        errorMessage.value = '邀请码无效，无法注册'
        return
      }
    }
  }

  isLoading.value = true
  try {
    const affCode = formData.aff_code.trim() || resolveGuestAffiliateCode()

    if (settings.email_verify_enabled) {
      sessionStorage.setItem(
        'register_data',
        JSON.stringify({
          email: formData.email,
          password: formData.password,
          turnstile_token: turnstileToken.value,
          promo_code: formData.promo_code || undefined,
          invitation_code: formData.invitation_code || undefined,
          ...(affCode ? { aff_code: affCode } : {}),
        }),
      )
      await router.push('/email-verify')
      return
    }

    await registerGuest({
      email: formData.email,
      password: formData.password,
      turnstile_token: settings.turnstile_enabled ? turnstileToken.value : undefined,
      promo_code: formData.promo_code || undefined,
      invitation_code: formData.invitation_code || undefined,
      ...(affCode ? { aff_code: affCode } : {}),
    })
    clearGuestAffiliateCode()
    toast.showSuccess(`账号创建成功，欢迎使用 ${GUEST_SITE_NAME}`)
    await navigateToAuthenticatedApp(router, `/${'dashboard'}`)
  } catch (error) {
    turnstileRef.value?.reset()
    turnstileToken.value = ''
    errorMessage.value = getGuestErrorMessage(error, '注册失败')
    toast.showError(errorMessage.value)
  } finally {
    isLoading.value = false
  }
}
</script>

<style scoped>
.fade-enter-active,
.fade-leave-active {
  transition: all 0.3s ease;
}

.fade-enter-from,
.fade-leave-to {
  opacity: 0;
  transform: translateY(-8px);
}

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

.guest-input-valid,
.guest-input-valid:focus {
  border-color: #10b981;
  box-shadow: 0 0 0 3px rgba(16, 185, 129, 0.12);
}
</style>
