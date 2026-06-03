import {defineConfig} from 'viteless'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
    plugins: [vue()],
    resolve: {
        alias: {'@': './src'},
    },
    server: {
        proxy: {'/api': 'http://localhost:8080'},
    },
    build: {outDir: 'dist'},
})
