<template>
  <h1 class="title">{{ msg }}</h1>
</template>

<script setup lang="ts">
import { ref } from "vue";
const msg = ref("hello from vue");
</script>

<style scoped>
.title { color: teal; }
</style>
