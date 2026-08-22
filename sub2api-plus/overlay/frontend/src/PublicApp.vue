<script setup lang="ts">
import { RouterView, useRoute } from 'vue-router'
import { watch } from 'vue'
import NavigationProgress from '@/components/common/NavigationProgress.vue'
import GuestToast from '@/components/guest/GuestToast.vue'

const route = useRoute()
const GUEST_SITE_NAME = '企业数据中台'

function updateDocumentTitle() {
  const title = route.meta.title
  document.title = typeof title === 'string' && title.trim()
    ? `${title.trim()} - ${GUEST_SITE_NAME}`
    : GUEST_SITE_NAME
}

watch(
  [
    () => route.fullPath,
    () => route.meta.title,
  ],
  updateDocumentTitle,
  { immediate: true }
)
</script>

<template>
  <NavigationProgress />
  <RouterView />
  <GuestToast />
</template>
