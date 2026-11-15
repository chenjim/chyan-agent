// Login view: paste the daemon shared token, stored in localStorage.

import { brandMarkSvg } from '../brand';
import type { View } from '../render';

export function loginView(onSubmit: (token: string) => void): View {
  return {
    html: `
      <div class="login">
        <span class="login-mark" aria-hidden="true">${brandMarkSvg('login')}</span>
        <h1 class="login-title">chyan</h1>
        <p class="login-kicker">呈言智 · 个人 Agent 工作台</p>
        <p class="login-hint">输入 daemon 启动配置的共享密钥（token，见 --token 或 CHYAN_TOKEN）以连接。</p>
        <form id="login-form" autocomplete="off">
          <input id="login-token" class="input mono" type="password"
                 placeholder="token" autocapitalize="off" autocorrect="off" required />
          <button class="btn primary big" type="submit">连接</button>
        </form>
      </div>`,
    mounted(root) {
      const form = root.querySelector<HTMLFormElement>('#login-form');
      form?.addEventListener('submit', (e) => {
        e.preventDefault();
        const token = root.querySelector<HTMLInputElement>('#login-token')?.value.trim() ?? '';
        if (token) onSubmit(token);
      });
    },
  };
}
