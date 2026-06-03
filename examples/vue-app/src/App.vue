<template>
  <main class="app">
    <h1>👋 Hello from <span class="brand">viteless</span> + Vue..</h1>
    <p class="hint">
      Edit <code>src/App.vue</code> or <code>src/components/Counter.vue</code> and
      save — the counter value below should survive the hot update (state-preserving HMR)...
    </p>
    <Counter :step="1" />
  </main>
</template>

<script setup lang="ts">
// "@/" is aliased to ./src by vite.config.ts — read natively by viteless.
import Counter from "@/components/Counter.vue";
</script>

<style scoped>
.app {
  max-width: 40rem;
  margin: 4rem auto;
  font-family: system-ui, sans-serif;
}
.brand { color: #41b883; }
.hint { color: #555; line-height: 1.5; }
code { background: #f0f0f0; padding: 0.1rem 0.35rem; border-radius: 4px; }
</style>
