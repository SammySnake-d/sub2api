<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { MirasimPoolControls } from './mirasimPoolControls'
const { t } = useI18n()
const props = defineProps<{ modelValue: MirasimPoolControls }>()
const emit = defineEmits<{ 'update:modelValue': [value: MirasimPoolControls] }>()
function set(key: keyof MirasimPoolControls, event: Event) {
  const value = Number((event.target as HTMLInputElement).value)
  emit('update:modelValue', { ...props.modelValue, [key]: value })
}
</script>

<template>
  <section data-testid="mirasim-pool-controls" class="space-y-4 border-t border-gray-200 pt-4 dark:border-dark-600">
    <h3 class="input-label text-base font-semibold">Mira · {{ t('admin.accounts.quotaControl.title') }}</h3>
    <p class="input-hint">{{ t('admin.accounts.quotaControl.mirasimHint') }}</p>
    <div class="grid gap-4 md:grid-cols-2">
      <label class="input-label">
        {{ t('admin.accounts.quotaControl.rpmLimit.baseRpm') }}
        <input data-testid="mirasim-base-rpm" type="number" class="input mt-1" min="0" max="10000" step="1" :value="modelValue.baseRPM" @input="set('baseRPM', $event)" />
      </label>
      <label class="input-label">
        {{ t('admin.accounts.quotaControl.sessionLimit.maxSessions') }}
        <input data-testid="mirasim-max-sessions" type="number" class="input mt-1" min="0" max="1000" step="1" :value="modelValue.maxSessions" @input="set('maxSessions', $event)" />
      </label>
      <label class="input-label">
        {{ t('admin.accounts.quotaControl.sessionLimit.idleTimeout') }} (min)
        <input data-testid="mirasim-idle-minutes" type="number" class="input mt-1" min="1" max="1440" step="1" :value="modelValue.idleMinutes" @input="set('idleMinutes', $event)" />
      </label>
      <label class="input-label">
        {{ t('admin.accounts.quotaControl.windowCost.limit') }} (USD)
        <input data-testid="mirasim-window-cost" type="number" class="input mt-1" min="0" step="0.01" :value="modelValue.windowCost" @input="set('windowCost', $event)" />
      </label>
      <label class="input-label">
        {{ t('admin.accounts.quotaControl.windowCost.stickyReserve') }} (USD)
        <input data-testid="mirasim-sticky-reserve" type="number" class="input mt-1" min="0" step="0.01" :value="modelValue.stickyReserve" @input="set('stickyReserve', $event)" />
      </label>
    </div>
  </section>
</template>
