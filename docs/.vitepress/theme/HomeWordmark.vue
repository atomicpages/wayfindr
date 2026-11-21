<script setup lang="ts">
import { useData } from "vitepress";
import { VPImage } from "vitepress/theme";

const { theme } = useData();
</script>

<template>
  <div class="home-wordmark">
    <VPImage :image="theme.logo" width="1654" height="422" />
  </div>
</template>

<style scoped>
.home-wordmark {
  width: min(100%, 380px);
  margin-bottom: 24px;
}

.home-wordmark :deep(.VPImage) {
  width: 100%;
  height: auto;
}
</style>
