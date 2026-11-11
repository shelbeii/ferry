<template>
  <span class="ferry-logo" role="img" aria-label="Ferry">
    <img class="ferry-logo-light" src="/branding/ferry-light.png" alt="" aria-hidden="true" draggable="false" />
    <img class="ferry-logo-dark" src="/branding/ferry-dark.png" alt="" aria-hidden="true" draggable="false" />
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
.ferry-logo-dark,
:global(html.dark .ferry-logo-light) {
  display: none;
}
:global(html.dark .ferry-logo-dark) {
  display: block;
}
</style>
