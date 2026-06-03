<template>
  <div class="counter">
    <button @click="count -= step">−</button>
    <span class="value">{{ count }}</span>
    <button @click="count += step">+</button>
  </div>
</template>

<script setup lang="ts">
import { ref } from "vue";

defineProps<{ step: number }>();
const count = ref(0);
</script>

<style scoped>
.counter {
  display: flex;
  align-items: center;
  gap: 1rem;
  margin-top: 2rem;
}
.value {
  font-size: 2rem;
  font-variant-numeric: tabular-nums;
  min-width: 3rem;
  text-align: center;
}
button {
  font-size: 1.25rem;
  width: 2.5rem;
  height: 2.5rem;
  border: 1px solid #41b883;
  background: #fff;
  color: #41b883;
  border-radius: 8px;
  cursor: pointer;
}
button:hover { background: #41b883; color: #fff; }
</style>
