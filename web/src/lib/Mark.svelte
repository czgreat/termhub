<script lang="ts">
  // The product mark (the same drawing as public/icons/icon.svg), at any size.
  let { size = 28 }: { size?: number } = $props()
</script>

<svg width={size} height={size} viewBox="0 0 512 512" aria-hidden="true">
  <defs>
    <linearGradient id="th-mark-bg" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="#4f8ff7" /><stop offset="1" stop-color="#7c5cff" />
    </linearGradient>
  </defs>
  <rect width="512" height="512" rx="110" fill="url(#th-mark-bg)" />
  <rect x="64" y="96" width="384" height="320" rx="28" fill="#10151c" fill-opacity=".85" />
  <path d="M132 176 L212 256 L132 336" fill="none" stroke="#fff" stroke-width="40" stroke-linecap="round" stroke-linejoin="round" />
  <rect x="236" y="316" width="140" height="40" rx="8" fill="#d6dee8" />
</svg>
