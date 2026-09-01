(() => {
  'use strict'

  const customPath = '/custom/quota-auto-reset'
  const initialCustomRoute = location.pathname === customPath
  if (initialCustomRoute) {
    document.documentElement.classList.add('quota-auto-reset-boot')
  }
  let scheduled = false

  const style = document.createElement('style')
  style.textContent = `
    body.quota-auto-reset-route main { padding: 0 !important; }
    body.quota-auto-reset-route .custom-page-layout {
      height: calc(100vh - 64px) !important;
    }
    body.quota-auto-reset-route .custom-page-layout > .card {
      border: 0 !important;
      border-radius: 0 !important;
      box-shadow: none !important;
      background: transparent !important;
    }
    body.quota-auto-reset-route .toc-sidebar,
    body.quota-auto-reset-route .toc-toggle-btn {
      display: none !important;
    }
    body.quota-auto-reset-route .markdown-page-content {
      padding: 0 !important;
      overflow: hidden !important;
      line-height: 1 !important;
    }
    body.quota-auto-reset-route .markdown-page-content > p {
      width: 100% !important;
      height: 100% !important;
      margin: 0 !important;
    }
    body.quota-auto-reset-route .markdown-page-content iframe {
      display: block !important;
      width: 100% !important;
      height: 100% !important;
      min-height: 0 !important;
      border: 0 !important;
      border-radius: 0 !important;
      background: transparent !important;
    }
    #quota-auto-reset-subtitle {
      margin: 0 !important;
      color: #6b7280;
      font-size: 12px;
      line-height: 16px;
    }
    .dark #quota-auto-reset-subtitle { color: #94a3b8; }
    html.quota-auto-reset-boot header h1,
    html.quota-auto-reset-boot header h1 ~ p,
    html.quota-auto-reset-boot body main {
      visibility: hidden !important;
    }
  `
  document.head.appendChild(style)

  function isAdmin() {
    try {
      return JSON.parse(localStorage.getItem('auth_user') || 'null')?.role === 'admin'
    } catch {
      return false
    }
  }

  function placeMenuItem() {
    scheduled = false
    if (!document.body) return
    const isCustomRoute = location.pathname === customPath
    document.body.classList.toggle('quota-auto-reset-route', isCustomRoute)

    const existingSubtitle = document.getElementById('quota-auto-reset-subtitle')
    if (!isCustomRoute) {
      existingSubtitle?.remove()
      document.documentElement.classList.remove('quota-auto-reset-boot')
    } else {
      const routeHeading = [...document.querySelectorAll('header h1')].find(
        (heading) => heading.textContent.trim() === '自动重置',
      )
      if (routeHeading && !existingSubtitle) {
        const subtitle = document.createElement('p')
        subtitle.id = 'quota-auto-reset-subtitle'
        subtitle.className = 'text-xs text-gray-500 dark:text-dark-400'
        subtitle.textContent = '自动联动重置设置'
        routeHeading.insertAdjacentElement('afterend', subtitle)
      }
      if (routeHeading && document.querySelector('.markdown-page-content iframe')) {
        requestAnimationFrame(() => {
          document.documentElement.classList.remove('quota-auto-reset-boot')
        })
      }
    }
    if (!isAdmin()) return

    const accountItem = document.getElementById('sidebar-channel-manage')
    const customItem = [...document.querySelectorAll('a[href]')].find((item) => {
      try {
        return new URL(item.getAttribute('href'), location.origin).pathname === customPath
      } catch {
        return false
      }
    })
    if (!accountItem || !customItem) return

    customItem.id = 'sidebar-auto-reset'
    if (accountItem.nextElementSibling !== customItem) {
      accountItem.insertAdjacentElement('afterend', customItem)
    }
  }

  function schedulePlacement() {
    if (scheduled) return
    scheduled = true
    requestAnimationFrame(placeMenuItem)
  }

  new MutationObserver(schedulePlacement).observe(document.documentElement, {
    childList: true,
    subtree: true,
  })
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', schedulePlacement, { once: true })
  } else {
    schedulePlacement()
  }
  if (initialCustomRoute) {
    setTimeout(() => document.documentElement.classList.remove('quota-auto-reset-boot'), 3000)
  }
})()
