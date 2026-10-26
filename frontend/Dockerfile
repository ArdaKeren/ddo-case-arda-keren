# syntax=docker/dockerfile:1.7

# --- build: produce the static bundle ----------------------------------------
FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY . .
RUN npm test && npm run build

# --- runtime: nginx serves the bundle and proxies API + WebSocket ------------
FROM nginx:1.27-alpine AS runtime
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
HEALTHCHECK --interval=5s --timeout=3s --retries=12 CMD wget -qO /dev/null http://127.0.0.1/ || exit 1
