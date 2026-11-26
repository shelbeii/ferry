<script setup lang="ts">
import { computed } from 'vue'
import { appIconPath } from '../appIcon'
import { resolvedTheme } from '../state'

const src = computed(() => appIconPath(resolvedTheme.value))
</script>

<template>
  <span class="ferry-logo" role="img" aria-label="Ferry">
    <img :src="src" alt="" aria-hidden="true" draggable="false" />
  </span>
</template>

<style scoped>
.ferry-logo {
  display: inline-flex;
  flex-shrink: 0;
  vertical-align: middle;
}
.ferry-logo img {
  width: 100%;
  height: 100%;
  object-fit: contain;
}
</style>
