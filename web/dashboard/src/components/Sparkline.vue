<script setup lang="ts">
import type { MetricPoint } from "../model";

const props = defineProps<{ first: MetricPoint[]; second?: MetricPoint[]; label?: string }>();
const width = 640;
const height = 170;
const all = [...props.first, ...(props.second || [])];
const max = Math.max(1, ...all.map((point) => point.value));

function line(points: MetricPoint[]): string {
  return points.map((point, index) => {
    const x = points.length <= 1 ? 0 : (index / (points.length - 1)) * width;
    const y = height - (point.value / max) * (height - 12) - 6;
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  }).join(" ");
}
</script>

<template>
  <svg class="sparkline" viewBox="0 0 640 170" role="img" :aria-label="label || 'metric trend'">
    <path class="grid-line" d="M0 42.5H640 M0 85H640 M0 127.5H640" />
    <polyline v-if="second" class="line-out" :points="line(second)" />
    <polyline class="line-in" :points="line(first)" />
  </svg>
</template>
