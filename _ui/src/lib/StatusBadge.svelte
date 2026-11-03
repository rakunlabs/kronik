<script lang="ts">
  import type { RunStatus } from './api'
  import { statusLabel, statusTone } from './format'

  let { status }: { status: RunStatus } = $props()
</script>

<span class="badge {statusTone[status]}">
  <span class="dot" class:live={status === 'running'}></span>
  {statusLabel[status]}
</span>
