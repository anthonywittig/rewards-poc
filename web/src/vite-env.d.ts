/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_API_BASE?: string
  readonly VITE_API_PROXY_TARGET?: string
  readonly VITE_TEMPORAL_UI_URL?: string
  readonly VITE_TEMPORAL_NAMESPACE?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
