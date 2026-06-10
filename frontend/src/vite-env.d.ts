/// <reference types="vite-plus/client" />

interface ImportMetaEnv {
  readonly VITE_BUILD_COMMIT: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
