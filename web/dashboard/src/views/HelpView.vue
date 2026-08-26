<script setup lang="ts">
import { Card, Divider, Space, Tag } from "@arco-design/web-vue";
</script>

<template>
  <div class="page-stack help-page">
    <div class="page-heading">
      <div><p class="eyebrow">DOCUMENTATION</p><h2>帮助中心</h2><p class="muted">保留技术术语，给每个概念一个可执行的下一步。</p></div>
      <Tag color="arcoblue">随版本发布</Tag>
    </div>

    <Card id="quick-start" :bordered="false" class="help-card">
      <template #title>快速开始</template>
      <ol class="guide-steps">
        <li><strong>init</strong><span>生成或准备 Gateway / Agent Bundle，并确认 token、证书和地址。</span></li>
        <li><strong>up</strong><span>启动角色，先看概览页的连接状态，再确认事件流为实时连接。</span></li>
        <li><strong>Dashboard</strong><span>使用 Viewer token 查看状态；需要 Apply/Rollback 时再输入 Admin token。</span></li>
      </ol>
    </Card>

    <Card id="concepts" :bordered="false" class="help-card">
      <template #title>概念速查</template>
      <div class="concept-grid">
        <article><Tag color="arcoblue">Gateway</Tag><p>公网入口进程，负责接收 Agent 连接和监听 reverse mapping 端口。</p><small>类比：前台接待。</small></article>
        <article><Tag color="arcoblue">Agent</Tag><p>内网节点进程，主动连接 Gateway，并把本地服务注册为 reverse mapping。</p><small>类比：内勤。</small></article>
        <article><Tag color="green">Reverse mapping</Tag><p>把 Agent 的内网地址连接到 Gateway 端口的配置。</p><small>类比：前台转接到内部分机。</small></article>
        <article><Tag color="orange">Egress</Tag><p>Agent 通过 Gateway 主动访问外部目标时使用的权限策略。</p><small>类比：出门审批。</small></article>
        <article><Tag color="purple">Inbound</Tag><p>Agent 本地监听的 SOCKS5 或 HTTP 代理入口。</p><small>类比：本机接待窗口。</small></article>
        <article><Tag color="cyan">Bundle</Tag><p>包含双角色配置、证书和 token 路径的一份可部署目录。</p><small>类比：开箱即用的套装。</small></article>
      </div>
    </Card>

    <Card id="operations" :bordered="false" class="help-card">
      <template #title>操作指南</template>
      <Space direction="vertical" fill>
        <div class="instruction-row"><span class="instruction-number">1</span><div><strong>查看服务</strong><p>进入“内网服务”，确认 local 地址、Gateway bind、端口和在线状态。</p></div></div>
        <div class="instruction-row"><span class="instruction-number">2</span><div><strong>编辑配置</strong><p>进入“设置”，基础项使用表单，reverse mappings 和复杂 ACL 使用 Advanced YAML。</p></div></div>
        <div class="instruction-row"><span class="instruction-number">3</span><div><strong>验证并应用</strong><p>先用 Viewer token Validate and preview；确认 diff 后点击 Apply，按提示输入 Admin token。</p></div></div>
      </Space>
    </Card>

    <Card id="faq" :bordered="false" class="help-card">
      <template #title>常见问题 FAQ</template>
      <div class="faq-list">
        <details open><summary>Dashboard 连不上怎么办？</summary><p>确认 management listener 可访问、Viewer token 正确，并检查 `/healthz` 与 `/readyz`。远程访问优先使用 SSH port forwarding。</p></details>
        <details><summary>token 在哪里？</summary><p>生成 Bundle 时位于对应角色的 <code>secrets/</code> 目录。Viewer token 用于查看，Admin token 只用于写操作。</p></details>
        <details><summary>为什么不能在“内网服务”里新建？</summary><p>当前服务配置属于 Agent 本地配置，Gateway 没有跨节点修改 Agent 配置的通道。请在 Agent 配置中修改 <code>agent.reverse</code>，再通过设置页或 CLI Apply。</p></details>
        <details><summary>怎么停止服务？</summary><p>使用 CLI 或受保护的 Admin action；Dashboard 当前只负责展示运行状态和配置管理。</p></details>
      </div>
    </Card>

    <Divider />
    <p class="muted help-footer">术语和操作随 AsterFerry 版本一起更新。遇到不确定的状态，先查看概览页的事件流。</p>
  </div>
</template>
