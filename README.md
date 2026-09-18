# ORSO Navegador

Navegador web nativo de escritorio construido en **Go puro** usando **WebView2** (Chromium embebido de Windows) y con **Tor integrado dentro del propio ejecutable** para navegación anónima.

El resultado final es un **único `.exe` portátil y autocontenido**: incluye el navegador, Tor y todas sus dependencias. No requiere instalar nada adicional.

---

## Características

- **Navegador nativo real**: cada URL se abre en una **ventana WebView2 independiente** (no iframes), por lo que funciona con cualquier sitio moderno (Google, YouTube, Wikipedia, etc.).
- **Hub central**: ventana principal con barra de dirección, buscador inteligente y accesos rápidos.
- **Tor incrustado en el ejecutable** (`//go:embed`): el binario `tor.exe` y los archivos `geoip` viajan dentro del `.exe` y se extraen a la caché del usuario la primera vez que se ejecuta.
- **Modo anónimo automático**: al arrancar, el navegador sale **por Tor** sin que el usuario tenga que hacer nada. Un badge morado en la barra inferior lo indica en todo momento.
- **DNS propios**: en modo normal, las resoluciones se hacen contra servidores públicos (`1.1.1.1`, `8.8.8.8`, `9.9.9.9`…) ignorando por completo el DNS del sistema operativo. En modo anónimo, el DNS lo resuelve el **nodo de salida de Tor** (sin fugas).
- **Proxy inteligente local**: un proxy HTTP CONNECT escrito en Go decide, conexión a conexión, si el tráfico va directo o por Tor según el modo activo.
- **Autenticación por cookie** en el puerto de control de Tor (sin warning de seguridad).
- **Sin ventana de consola**: los logs van a archivo, no a stdout. El `.exe` se compila con `-H windowsgui`.
- **Cierre limpio**: al cerrar, se apaga Tor con `SIGNAL SHUTDOWN`, se cierra el proxy y todas sus conexiones, se libera el listener, se borra el `lock` y se fuerza `GC` + `FreeOSMemory`.
- **Detección de Tor huérfano**: si una ejecución anterior dejó un `tor.exe` colgado, la app lo detecta y lo mata antes de arrancar el suyo.

---

## Capturas

*(Añade aquí tus capturas de pantalla — `/docs/screenshots/hub.png`, `/docs/screenshots/anonimo.png`, etc.)*

---

## Requisitos

### Para ejecutar el `.exe`

- **Windows 10/11** de 64 bits.
- **Microsoft Edge WebView2 Runtime** — viene preinstalado en Windows 10/11 modernos. Si no lo tienes, descárgalo desde [aquí](https://developer.microsoft.com/microsoft-edge/webview2/).

No hace falta instalar Node, Go, Tor ni nada más.

### Para compilar desde el código fuente

- **Go ≥ 1.21**.
- **MinGW-w64** (compilador C, necesario porque `webview_go` usa CGO). En Windows puedes instalarlo con:
  ```powershell
  choco install mingw
  ```
- **CGO habilitado**:
  ```powershell
  go env -w CGO_ENABLED=1
  ```
- **Tor Expert Bundle** (solo en tiempo de compilación) — ver sección siguiente.

---

## Estructura del proyecto

```
orso-navegador/
├── main.go                  # TODO el programa (un solo archivo)
├── go.mod
├── go.sum
├── tor/                     # Solo para compilar. Se incrusta en el .exe
│   ├── tor.exe              # Obligatorio (Tor Expert Bundle 15.x)
│   ├── geoip                # Opcional
│   └── geoip6               # Opcional
└── docs/
    └── screenshots/         # Capturas para el README
```

El `.exe` final **no necesita** la carpeta `tor/`. La lleva dentro.

---

## Obtener Tor (solo para compilar)

1. Ve a https://www.torproject.org/download/tor/
2. Descarga **Windows Expert Bundle (x86_64)**.
3. Descomprime el `.tar.gz` (con 7-Zip o similar).
4. Copia los archivos a la carpeta `tor/` junto a `main.go`:
   ```
   tor/
   ├── tor.exe
   ├── geoip
   └── geoip6
   ```

> `tor.exe` es **obligatorio**. `geoip` y `geoip6` son opcionales: si están, se incrustan; si no, la app funciona igual pero sin geolocalización de relays en los logs.

---

## Compilar el ejecutable

```powershell
# 1) Inicializar el módulo (una sola vez)
go mod init orso-navegador

# 2) Traer la dependencia de WebView
go get github.com/webview/webview_go

# 3) Compilar el .exe único y autocontenido
go build -ldflags="-H windowsgui -s -w" -o ORSO-Navegador.exe main.go
```

El `.exe` resultante (≈ 40–50 MB) se puede copiar a cualquier Windows 10/11 y ejecutar con doble clic.

---

## Uso

### Al arrancar

1. Doble clic en `ORSO-Navegador.exe`.
2. Se abre el **hub** con barra inferior mostrando el badge **`🕶️ MODO ANÓNIMO`**.
3. Tor empieza a bootstrapear en segundo plano. La barra muestra `Tor: 0% → 15% → 50% → listo`. La primera vez tarda 30–60 s; las siguientes, 5–10 s.
4. Cuando diga `Tor: listo`, ya puedes navegar.

### Navegar

- Escribe en la barra una URL (`github.com`) o un texto para buscar en Google.
- Cada URL se abre en una **ventana nativa de ORSO** con su propia barra de título.
- En modo anónimo, las ventanas llevan el prefijo **🕶️** en el título.
- Botón **IP** verifica `check.torproject.org` y muestra si sales por Tor.
- Botón **🕶️** alterna entre modo anónimo y normal. El badge de la barra inferior cambia de color.

### Ajustes (⚙️)

- Estado de Tor (porcentaje, mensaje, listo/no listo).
- Botones: Iniciar / Reiniciar / Detener Tor.
- Lista de servidores DNS públicos activos.

### Atajos de teclado

| Acción | Atajo |
|---|---|
| Enfocar barra de URL | `Ctrl+L` |
| Alternar modo anónimo | `Ctrl+Shift+N` |
| Pantalla completa | `F11` |

---

## Flags de línea de comandos

| Flag | Descripción | Ejemplo |
|---|---|---|
| `-data` | Directorio de datos de Tor (por defecto: caché del usuario) | `-data "D:\tor-data"` |
| `-dns` | Servidores DNS públicos separados por coma | `-dns "1.1.1.1:53,9.9.9.9:53"` |
| `-no-anon` | Arrancar en modo normal (sin Tor) | `-no-anon` |

Ejemplos:

```powershell
# Modo anónimo (por defecto) con DNS personalizados
.\ORSO-Navegador.exe -dns "1.1.1.1:53,9.9.9.9:53"

# Arrancar en modo normal
.\ORSO-Navegador.exe -no-anon
```

---

## Cómo funciona por dentro

### Arquitectura

```
┌──────────────────────────────────────────────────────────┐
│                    ORSO-Navegador.exe                    │
│                                                          │
│  ┌────────────────┐        ┌──────────────────────┐     │
│  │  Ventana hub   │        │ Ventanas WebView2    │     │
│  │  (UI en HTML)  │───────▶│ (una por URL)        │     │
│  └────────────────┘        └──────────┬───────────┘     │
│                                       │                 │
│                                       ▼                 │
│                          ┌──────────────────────┐       │
│                          │  SmartProxy (Go)     │       │
│                          │  127.0.0.1:19060     │       │
│                          └───┬───────────┬──────┘       │
│                              │           │              │
│                    modo normal│           │modo anon     │
│                              ▼           ▼              │
│                    DNS propios      SOCKS5 a Tor        │
│                    (1.1.1.1…)      127.0.0.1:19050      │
│                                              │          │
│                                              ▼          │
│                                    ┌──────────────┐     │
│                                    │  tor.exe     │     │
│                                    │ (incrustado) │     │
│                                    └──────────────┘     │
└──────────────────────────────────────────────────────────┘
```

### Tor incrustado

- El binario `tor.exe` y `geoip*` se incrustan con `//go:embed all:tor`.
- Al arrancar, se extraen a `%LOCALAPPDATA%\orso-navegador\tor-<hash>\` (una sola vez por versión).
- El hash del `tor.exe` se usa como nombre de carpeta: al actualizar el binario, se reextrae solo.
- Los puertos usados son **dedicados** para no chocar con un Tor del sistema:
  - SOCKS: `127.0.0.1:19050`
  - Control: `127.0.0.1:19051`

### DNS propios

- **Modo normal**: `dialWithCustomDNS()` consulta directamente a `1.1.1.1`, `8.8.8.8`, `9.9.9.9`, etc., ignorando el resolver del sistema. Si todos fallan, cae al resolver del SO como red de seguridad (queda registrado en el log).
- **Modo anónimo**: el hostname se envía **tal cual** a Tor por SOCKS5 (tipo dominio). El nodo de salida de Tor resuelve el DNS dentro de la red Tor. **Cero fugas de DNS**.

### Logs

Toda la actividad se escribe a archivo (nada sale por consola):

```
%LOCALAPPDATA%\orso-navegador\logs\orso.log    ← eventos de la app
%LOCALAPPDATA%\orso-navegador\logs\tor.log     ← salida cruda de tor.exe
```

Para verlos en vivo:

```powershell
Get-Content "$env:LOCALAPPDATA\orso-navegador\logs\orso.log" -Wait -Tail 20
```

---

## Limitaciones conocidas

- **CAPTCHA de Google y otros servicios**: al usar Tor, muchas webs (Google, Cloudflare, etc.) muestran CAPTCHA o bloquean peticiones porque la IP del nodo de salida es compartida por miles de usuarios. **No es un fallo del navegador** — es inherente a usar Tor. Soluciones: cambiar el circuito de Tor, usar DuckDuckGo/Brave, o resolver el CAPTCHA puntualmente.
- **WebView2 sin soporte de pestañas internas**: WebView2 no expone una API de pestañas. Por eso cada URL se abre en una ventana nativa del sistema operativo. Es una decisión de diseño, no una carencia.
- **Modo anónimo global, no por pestaña**: al ser WebView2 quien decide el proxy a nivel de proceso, el modo (normal/anónimo) se aplica a todas las conexiones nuevas. Cambiar el modo mientras una página está cargada requiere recargarla.
- **Sin firma de código**: el `.exe` no está firmado, por lo que Windows SmartScreen puede mostrar un aviso la primera vez. Pulsa *Más información → Ejecutar de todas formas*.

---

## Avisos de seguridad

- **Proxies públicos**: si en el futuro se añaden, recuerda que son inestables y **no cifran** tu tráfico. La opción segura es **Tor**.
- **Anonimato real**: Tor oculta tu IP, pero el anonimato total depende del uso. No inicies sesión en servicios personales mientras navegas en modo anónimo. Consulta las [recomendaciones de seguridad de Tor](https://support.torproject.org/faq/staying-anonymous/).
- **Antivirus / SmartScreen**: la extracción de un `tor.exe` desde un binario no firmado puede activar heurísticas de Windows Defender. Es esperable. Añade una exclusión si es necesario.

---

## Licencia

MIT. Tor es un proyecto de [The Tor Project](https://www.torproject.org/) y se distribuye según sus propios términos.
