/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_API_PROXY_TARGET?: string
  readonly VITE_TEMPORAL_UI_URL?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
