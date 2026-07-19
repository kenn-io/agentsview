---
title: AgentsView
description: Local-first desktop and web app for AI agent sessions
---

# AgentsView

A local-first desktop and web app for browsing, searching, and analyzing your
past AI coding sessions. See where your agents' time and money actually go —
across every project, model, and tool.

<p class="hero-actions">
  <a class="md-button md-button--primary" href="/quickstart/">Get Started</a>
  <a class="md-button" href="https://github.com/kenn-io/agentsview">View on GitHub</a>
</p>

<figure class="hero-shot" data-lightbox>
  <img src="/assets/generated/screenshots/dashboard.png" alt="AgentsView analytics dashboard" loading="eager" />
</figure>

<svg class="agent-sprite" aria-hidden="true" focusable="false" width="0" height="0" style="position:absolute">
  <symbol id="i-claude" viewBox="0 0 24 24"><path d="m4.7144 15.9555 4.7174-2.6471.079-.2307-.079-.1275h-.2307l-.7893-.0486-2.6956-.0729-2.3375-.0971-2.2646-.1214-.5707-.1215-.5343-.7042.0546-.3522.4797-.3218.686.0608 1.5179.1032 2.2767.1578 1.6514.0972 2.4468.255h.3886l.0546-.1579-.1336-.0971-.1032-.0972L6.973 9.8356l-2.55-1.6879-1.3356-.9714-.7225-.4918-.3643-.4614-.1578-1.0078.6557-.7225.8803.0607.2246.0607.8925.686 1.9064 1.4754 2.4893 1.8336.3643.3035.1457-.1032.0182-.0728-.164-.2733-1.3539-2.4467-1.445-2.4893-.6435-1.032-.17-.6194c-.0607-.255-.1032-.4674-.1032-.7285L6.287.1335 6.6997 0l.9957.1336.419.3642.6192 1.4147 1.0018 2.2282 1.5543 3.0296.4553.8985.2429.8318.091.255h.1579v-.1457l.1275-1.706.2368-2.0947.2307-2.6957.0789-.7589.3764-.9107.7468-.4918.5828.2793.4797.686-.0668.4433-.2853 1.8517-.5586 2.9021-.3643 1.9429h.2125l.2429-.2429.9835-1.3053 1.6514-2.0643.7286-.8196.85-.9046.5464-.4311h1.0321l.759 1.1293-.34 1.1657-1.0625 1.3478-.8804 1.1414-1.2628 1.7-.7893 1.36.0729.1093.1882-.0183 2.8535-.607 1.5421-.2794 1.8396-.3157.8318.3886.091.3946-.3278.8075-1.967.4857-2.3072.4614-3.4364.8136-.0425.0304.0486.0607 1.5482.1457.6618.0364h1.621l3.0175.2247.7892.522.4736.6376-.079.4857-1.2142.6193-1.6393-.3886-3.825-.9107-1.3113-.3279h-.1822v.1093l1.0929 1.0686 2.0035 1.8092 2.5075 2.3314.1275.5768-.3218.4554-.34-.0486-2.2039-1.6575-.85-.7468-1.9246-1.621h-.1275v.17l.4432.6496 2.3436 3.5214.1214 1.0807-.17.3521-.6071.2125-.6679-.1214-1.3721-1.9246L14.38 17.959l-1.1414-1.9428-.1397.079-.674 7.2552-.3156.3703-.7286.2793-.6071-.4614-.3218-.7468.3218-1.4753.3886-1.9246.3157-1.53.2853-1.9004.17-.6314-.0121-.0425-.1397.0182-1.4328 1.9672-2.1796 2.9446-1.7243 1.8456-.4128.164-.7164-.3704.0667-.6618.4008-.5889 2.386-3.0357 1.4389-1.882.929-1.0868-.0062-.1579h-.0546l-6.3385 4.1164-1.1293.1457-.4857-.4554.0608-.7467.2307-.2429 1.9064-1.3114Z"/></symbol>
  <symbol id="i-openai" viewBox="0 0 24 24"><path d="M22.2819 9.8211a5.9847 5.9847 0 0 0-.5157-4.9108 6.0462 6.0462 0 0 0-6.5098-2.9A6.0651 6.0651 0 0 0 4.9807 4.1818a5.9847 5.9847 0 0 0-3.9977 2.9 6.0462 6.0462 0 0 0 .7427 7.0966 5.98 5.98 0 0 0 .511 4.9107 6.051 6.051 0 0 0 6.5146 2.9001A5.9847 5.9847 0 0 0 13.2599 24a6.0557 6.0557 0 0 0 5.7718-4.2058 5.9894 5.9894 0 0 0 3.9977-2.9001 6.0557 6.0557 0 0 0-.7475-7.0729zm-9.022 12.6081a4.4755 4.4755 0 0 1-2.8764-1.0408l.1419-.0804 4.7783-2.7582a.7948.7948 0 0 0 .3927-.6813v-6.7369l2.02 1.1686a.071.071 0 0 1 .038.052v5.5826a4.504 4.504 0 0 1-4.4945 4.4944zm-9.6607-4.1254a4.4708 4.4708 0 0 1-.5346-3.0137l.142.0852 4.783 2.7582a.7712.7712 0 0 0 .7806 0l5.8428-3.3685v2.3324a.0804.0804 0 0 1-.0332.0615L9.74 19.9502a4.4992 4.4992 0 0 1-6.1408-1.6464zM2.3408 7.8956a4.485 4.485 0 0 1 2.3655-1.9728V11.6a.7664.7664 0 0 0 .3879.6765l5.8144 3.3543-2.0201 1.1685a.0757.0757 0 0 1-.071 0l-4.8303-2.7865A4.504 4.504 0 0 1 2.3408 7.872zm16.5963 3.8558L13.1038 8.364 15.1192 7.2a.0757.0757 0 0 1 .071 0l4.8303 2.7913a4.4944 4.4944 0 0 1-.6765 8.1042v-5.6772a.79.79 0 0 0-.407-.667zm2.0107-3.0231l-.142-.0852-4.7735-2.7818a.7759.7759 0 0 0-.7854 0L9.409 9.2297V6.8974a.0662.0662 0 0 1 .0284-.0615l4.8303-2.7866a4.4992 4.4992 0 0 1 6.6802 4.66zM8.3065 12.863l-2.02-1.1638a.0804.0804 0 0 1-.038-.0567V6.0742a4.4992 4.4992 0 0 1 7.3757-3.4537l-.142.0805L8.704 5.459a.7948.7948 0 0 0-.3927.6813zm1.0976-2.3654l2.602-1.4998 2.6069 1.4998v2.9994l-2.5974 1.4997-2.6067-1.4997Z"/></symbol>
  <symbol id="i-gemini" viewBox="0 0 24 24"><path d="M11.04 19.32Q12 21.51 12 24q0-2.49.93-4.68.96-2.19 2.58-3.81t3.81-2.55Q21.51 12 24 12q-2.49 0-4.68-.93a12.3 12.3 0 0 1-3.81-2.58 12.3 12.3 0 0 1-2.58-3.81Q12 2.49 12 0q0 2.49-.96 4.68-.93 2.19-2.55 3.81a12.3 12.3 0 0 1-3.81 2.58Q2.49 12 0 12q2.49 0 4.68.96 2.19.93 3.81 2.55t2.55 3.81"/></symbol>
  <symbol id="i-copilot" viewBox="0 0 24 24"><path d="M23.922 16.997C23.061 18.492 18.063 22.02 12 22.02 5.937 22.02.939 18.492.078 16.997A.641.641 0 0 1 0 16.741v-2.869a.883.883 0 0 1 .053-.22c.372-.935 1.347-2.292 2.605-2.656.167-.429.414-1.055.644-1.517a10.098 10.098 0 0 1-.052-1.086c0-1.331.282-2.499 1.132-3.368.397-.406.89-.717 1.474-.952C7.255 2.937 9.248 1.98 11.978 1.98c2.731 0 4.767.957 6.166 2.093.584.235 1.077.546 1.474.952.85.869 1.132 2.037 1.132 3.368 0 .368-.014.733-.052 1.086.23.462.477 1.088.644 1.517 1.258.364 2.233 1.721 2.605 2.656a.841.841 0 0 1 .053.22v2.869a.641.641 0 0 1-.078.256Zm-11.75-5.992h-.344a4.359 4.359 0 0 1-.355.508c-.77.947-1.918 1.492-3.508 1.492-1.725 0-2.989-.359-3.782-1.259a2.137 2.137 0 0 1-.085-.104L4 11.746v6.585c1.435.779 4.514 2.179 8 2.179 3.486 0 6.565-1.4 8-2.179v-6.585l-.098-.104s-.033.045-.085.104c-.793.9-2.057 1.259-3.782 1.259-1.59 0-2.738-.545-3.508-1.492a4.359 4.359 0 0 1-.355-.508Zm2.328 3.25c.549 0 1 .451 1 1v2c0 .549-.451 1-1 1-.549 0-1-.451-1-1v-2c0-.549.451-1 1-1Zm-5 0c.549 0 1 .451 1 1v2c0 .549-.451 1-1 1-.549 0-1-.451-1-1v-2c0-.549.451-1 1-1Zm3.313-6.185c.136 1.057.403 1.913.878 2.497.442.544 1.134.938 2.344.938 1.573 0 2.292-.337 2.657-.751.384-.435.558-1.15.558-2.361 0-1.14-.243-1.847-.705-2.319-.477-.488-1.319-.862-2.824-1.025-1.487-.161-2.192.138-2.533.529-.269.307-.437.808-.438 1.578v.021c0 .265.021.562.063.893Zm-1.626 0c.042-.331.063-.628.063-.894v-.02c-.001-.77-.169-1.271-.438-1.578-.341-.391-1.046-.69-2.533-.529-1.505.163-2.347.537-2.824 1.025-.462.472-.705 1.179-.705 2.319 0 1.211.175 1.926.558 2.361.365.414 1.084.751 2.657.751 1.21 0 1.902-.394 2.344-.938.475-.584.742-1.44.878-2.497Z"/></symbol>
  <symbol id="i-vscode" viewBox="0 0 24 24"><path d="M23.15 2.587L18.21.21a1.494 1.494 0 0 0-1.705.29l-9.46 8.63-4.12-3.128a.999.999 0 0 0-1.276.057L.327 7.261A1 1 0 0 0 .326 8.74L3.899 12 .326 15.26a1 1 0 0 0 .001 1.479L1.65 17.94a.999.999 0 0 0 1.276.057l4.12-3.128 9.46 8.63a1.492 1.492 0 0 0 1.704.29l4.942-2.377A1.5 1.5 0 0 0 24 20.06V3.939a1.5 1.5 0 0 0-.85-1.352zm-5.146 14.861L10.826 12l7.178-5.448v10.896z"/></symbol>
  <symbol id="i-visualstudio" viewBox="0 0 24 24"><path d="M17.583.063a1.5 1.5 0 00-1.032.392 1.5 1.5 0 00-.001 0A.88.88 0 0016.5.5L8.528 9.316 3.875 5.5l-.407-.35a1 1 0 00-1.024-.154 1 1 0 00-.012.005l-1.817.75a1 1 0 00-.077.036 1 1 0 00-.047.028 1 1 0 00-.038.022 1 1 0 00-.048.034 1 1 0 00-.03.024 1 1 0 00-.044.036 1 1 0 00-.036.033 1 1 0 00-.032.035 1 1 0 00-.033.038 1 1 0 00-.035.044 1 1 0 00-.024.034 1 1 0 00-.032.05 1 1 0 00-.02.035 1 1 0 00-.024.05 1 1 0 00-.02.045 1 1 0 00-.016.044 1 1 0 00-.016.047 1 1 0 00-.015.055 1 1 0 00-.01.04 1 1 0 00-.008.054 1 1 0 00-.006.05A1 1 0 000 6.668v10.666a1 1 0 00.615.917l1.817.764a1 1 0 001.035-.164l.408-.35 4.653-3.815 7.973 8.815a1.5 1.5 0 00.072.065 1.5 1.5 0 00.057.05 1.5 1.5 0 00.058.042 1.5 1.5 0 00.063.044 1.5 1.5 0 00.065.038 1.5 1.5 0 00.065.036 1.5 1.5 0 00.068.031 1.5 1.5 0 00.07.03 1.5 1.5 0 00.073.025 1.5 1.5 0 00.066.02 1.5 1.5 0 00.08.02 1.5 1.5 0 00.068.014 1.5 1.5 0 00.075.01 1.5 1.5 0 00.075.008 1.5 1.5 0 00.073.003 1.5 1.5 0 00.077 0 1.5 1.5 0 00.078-.005 1.5 1.5 0 00.067-.007 1.5 1.5 0 00.087-.015 1.5 1.5 0 00.06-.012 1.5 1.5 0 00.08-.022 1.5 1.5 0 00.068-.02 1.5 1.5 0 00.07-.028 1.5 1.5 0 00.09-.037l4.944-2.377a1.5 1.5 0 00.476-.362 1.5 1.5 0 00.09-.112 1.5 1.5 0 00.004-.007 1.5 1.5 0 00.08-.125 1.5 1.5 0 00.062-.12 1.5 1.5 0 00.009-.017 1.5 1.5 0 00.04-.108 1.5 1.5 0 00.015-.037 1.5 1.5 0 00.03-.107 1.5 1.5 0 00.009-.037 1.5 1.5 0 00.017-.1 1.5 1.5 0 00.008-.05 1.5 1.5 0 00.006-.09 1.5 1.5 0 00.004-.08V3.942a1.5 1.5 0 000-.003 1.5 1.5 0 000-.032 1.5 1.5 0 00-.01-.15 1.5 1.5 0 00-.84-1.17L18.206.21a1.5 1.5 0 00-.622-.146zM18 6.92v10.163l-6.198-5.08zM3 8.574l3.099 3.427-3.1 3.426z"/></symbol>
  <symbol id="i-cursor" viewBox="0 0 24 24"><path d="M11.503.131 1.891 5.678a.84.84 0 0 0-.42.726v11.188c0 .3.162.575.42.724l9.609 5.55a1 1 0 0 0 .998 0l9.61-5.55a.84.84 0 0 0 .42-.724V6.404a.84.84 0 0 0-.42-.726L12.497.131a1.01 1.01 0 0 0-.996 0M2.657 6.338h18.55c.263 0 .43.287.297.515L12.23 22.918c-.062.107-.229.064-.229-.06V12.335a.59.59 0 0 0-.295-.51l-9.11-5.257c-.109-.063-.064-.23.061-.23"/></symbol>
  <symbol id="i-warp" viewBox="0 0 24 24"><path d="M12.035 2.723h9.253A2.712 2.712 0 0 1 24 5.435v10.529a2.712 2.712 0 0 1-2.712 2.713H8.047Zm-1.681 2.6L6.766 19.677h5.598l-.399 1.6H2.712A2.712 2.712 0 0 1 0 18.565V8.036a2.712 2.712 0 0 1 2.712-2.712Z"/></symbol>
  <symbol id="i-qwen" viewBox="0 0 24 24"><path d="M23.919 14.545 20.817 9.17l1.47-2.544a.56.56 0 0 0 0-.566l-1.633-2.83a.57.57 0 0 0-.49-.283h-6.207L12.487.402a.57.57 0 0 0-.49-.284H8.732a.56.56 0 0 0-.49.284L5.139 5.775h-2.94a.56.56 0 0 0-.49.284L.077 8.887a.56.56 0 0 0 0 .567L3.18 14.83l-1.47 2.545a.56.56 0 0 0 0 .566l1.634 2.83a.57.57 0 0 0 .49.283h6.205l1.47 2.545a.57.57 0 0 0 .49.284h3.266a.57.57 0 0 0 .49-.284l3.104-5.375h2.94a.57.57 0 0 0 .49-.283l1.634-2.828a.55.55 0 0 0-.004-.568M8.733.686l1.634 2.828-1.634 2.828H21.8L20.164 9.17H7.425L5.63 6.06Zm1.306 19.801-6.205-.002 1.634-2.83h3.265L2.201 6.344h3.267q3.182 5.517 6.367 11.032zm10.124-5.66L18.53 12l-6.532 11.315-1.634-2.83c2.129-3.673 4.25-7.351 6.373-11.028h3.592l3.102 5.374z"/></symbol>
  <symbol id="i-deepseek" viewBox="0 0 24 24"><path d="M23.748 4.651c-.254-.124-.364.113-.512.233-.051.04-.094.09-.137.137-.372.397-.806.657-1.373.626-.829-.046-1.537.214-2.163.848-.133-.782-.575-1.248-1.247-1.548-.352-.155-.708-.311-.955-.65-.172-.24-.219-.509-.305-.774-.055-.16-.11-.323-.293-.35-.2-.031-.278.136-.356.276-.313.572-.434 1.202-.422 1.84.027 1.436.633 2.58 1.838 3.393.137.094.172.187.129.323-.082.28-.18.553-.266.833-.055.179-.137.218-.328.14a5.5 5.5 0 0 1-1.737-1.179c-.857-.828-1.631-1.743-2.597-2.46a12 12 0 0 0-.689-.47c-.985-.957.13-1.743.387-1.836.27-.098.094-.433-.778-.428-.872.003-1.67.295-2.687.685a3 3 0 0 1-.465.136 9.6 9.6 0 0 0-2.883-.101c-1.885.21-3.39 1.1-4.497 2.622C.082 8.776-.231 10.854.152 13.02c.403 2.284 1.568 4.175 3.36 5.653 1.857 1.533 3.997 2.284 6.438 2.14 1.482-.085 3.132-.284 4.994-1.86.47.234.962.328 1.78.398.629.058 1.235-.031 1.705-.129.735-.155.684-.836.418-.961-2.155-1.004-1.682-.595-2.112-.926 1.095-1.295 2.768-3.598 3.284-6.733.05-.346.115-.834.108-1.114-.004-.171.035-.238.23-.257a4.2 4.2 0 0 0 1.545-.475c1.397-.763 1.96-2.016 2.093-3.517.02-.23-.004-.467-.247-.588M11.58 18.168c-2.088-1.642-3.101-2.183-3.52-2.16-.39.024-.32.472-.234.763.09.288.207.487.371.74.114.167.192.416-.113.603-.673.416-1.842-.14-1.897-.168-1.361-.801-2.5-1.86-3.301-3.306-.775-1.393-1.225-2.888-1.299-4.482-.02-.385.094-.522.477-.592a4.7 4.7 0 0 1 1.53-.038c2.131.311 3.946 1.264 5.467 2.774.868.86 1.525 1.887 2.202 2.89.72 1.066 1.494 2.082 2.48 2.915.348.291.626.513.892.677-.802.09-2.14.109-3.055-.615zm1.001-6.44a.306.306 0 0 1 .415-.287.3.3 0 0 1 .113.074.3.3 0 0 1 .086.214c0 .17-.136.307-.308.307a.303.303 0 0 1-.306-.307m3.11 1.596c-.2.081-.4.151-.591.16a1.25 1.25 0 0 1-.798-.254c-.274-.23-.47-.358-.551-.758a1.7 1.7 0 0 1 .015-.588c.07-.327-.007-.537-.238-.727-.188-.156-.426-.199-.689-.199a.6.6 0 0 1-.254-.078.253.253 0 0 1-.114-.358 1 1 0 0 1 .192-.21c.356-.202.767-.136 1.146.016.352.144.618.408 1.001.782.392.451.462.576.685.915.176.264.336.536.446.848.066.194-.02.353-.25.45"/></symbol>
  <symbol id="i-mistral" viewBox="0 0 24 24"><path d="M17.143 3.429v3.428h-3.429v3.429h-3.428V6.857H6.857V3.43H3.43v13.714H0v3.428h10.286v-3.428H6.857v-3.429h3.429v3.429h3.429v-3.429h3.428v3.429h-3.428v3.428H24v-3.428h-3.43V3.429z"/></symbol>
  <symbol id="i-zed" viewBox="0 0 24 24"><path d="M2.25 1.5a.75.75 0 0 0-.75.75v16.5H0V2.25A2.25 2.25 0 0 1 2.25 0h20.095c1.002 0 1.504 1.212.795 1.92L10.764 14.298h3.486V12.75h1.5v1.922a1.125 1.125 0 0 1-1.125 1.125H9.264l-2.578 2.578h11.689V9h1.5v9.375a1.5 1.5 0 0 1-1.5 1.5H5.185L2.562 22.5H21.75a.75.75 0 0 0 .75-.75V5.25H24v16.5A2.25 2.25 0 0 1 21.75 24H1.655C.653 24 .151 22.788.86 22.08L13.19 9.75H9.75v1.5h-1.5V9.375A1.125 1.125 0 0 1 9.375 8.25h5.314l2.625-2.625H5.625V15h-1.5V5.625a1.5 1.5 0 0 1 1.5-1.5h13.19L21.438 1.5z"/></symbol>
  <symbol id="i-posit" viewBox="0 0 24 24"><path d="M0 .953v6.393l4.852 2.066-3.27 1.447v2.283l3.215 1.432L0 16.615v6.432l11.918-5.256.082-.035.082.035L24 23.047v-6.432l-4.797-2.04 3.215-1.433v-2.283l-3.27-1.447L24 7.346V.953L12 6.25Zm.879 1.352 10.039 4.431-4.96 2.19L.879 6.763Zm22.242 0v4.458l-5.066 2.162-4.973-2.19 10.04-4.431ZM12 7.209l4.945 2.19-4.95 2.107-4.94-2.108zM5.959 9.885l4.926 2.093-.006.002.006.002-4.979 2.12-3.446-1.529v-1.148l3.5-1.541zm12.082 0 3.514 1.54v1.15l-3.448 1.526-1.107.487-4.994 2.21L7 14.589l4.994-2.133L17 14.588l1.094-.487-4.973-2.12zM5.906 15.06l5.012 2.215-.066.03-9.973 4.404v-4.512zm12.201 0 5.014 2.137v4.512l-9.959-4.404-.066-.03z"/></symbol>
  <symbol id="i-sourcegraph" viewBox="0 0 24 24"><path d="M17.897 3.84a2.38 2.38 0 1 1 3.09 3.623l-3.525 3.006-2.59-.919-.967-.342-1.625-.576 1.312-1.12.78-.665 3.525-3.007zm-8.27 13.313.78-.665 1.312-1.12-1.624-.575-.967-.344-2.59-.918-3.525 3.007a2.38 2.38 0 1 0 3.09 3.622l3.525-3.007zM8.724 7.37l2.592.92 2.09-1.784-.84-4.556a2.38 2.38 0 1 0-4.683.865l.841 4.555zm6.554 9.262-2.592-.92-2.091 1.784.842 4.557a2.38 2.38 0 0 0 4.682-.866l-.841-4.555zm8.186-.564a2.38 2.38 0 0 0-1.449-3.04l-4.365-1.55-.967-.342-1.625-.576-.966-.343-2.59-.92-.967-.342-1.624-.576-.967-.343-4.366-1.55a2.38 2.38 0 1 0-1.591 4.488l4.366 1.55.966.342 1.625.576.965.343 2.591.92.967.342 1.624.577.966.342 4.367 1.55a2.38 2.38 0 0 0 3.04-1.447"/></symbol>
  <symbol id="i-opencode" viewBox="0 0 24 24"><path d="M22 24H2V0h20zM17 4.8H7v14.4h10z"/></symbol>
</svg>

<p class="agent-section__lead">Reads sessions from dozens of AI coding agents &mdash; auto-discovered, nothing to configure.</p>

<div class="agent-grid">
  <a class="agent-chip" data-agent="claude-code" href="https://www.anthropic.com/claude-code" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-claude"/></svg></span><span class="agent-chip__name">Claude Code</span></a>
  <a class="agent-chip" data-agent="openclaude" href="/configuration/#session-discovery"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-claude"/></svg></span><span class="agent-chip__name">OpenClaude</span></a>
  <a class="agent-chip" data-agent="codex" href="https://openai.com/codex/" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-openai"/></svg></span><span class="agent-chip__name">Codex</span></a>
  <a class="agent-chip" data-agent="gemini" href="https://github.com/google-gemini/gemini-cli" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-gemini"/></svg></span><span class="agent-chip__name">Gemini</span></a>
  <a class="agent-chip" data-agent="copilot" href="https://github.com/features/copilot" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-copilot"/></svg></span><span class="agent-chip__name">Copilot</span></a>
  <a class="agent-chip" data-agent="cursor" href="https://cursor.com" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-cursor"/></svg></span><span class="agent-chip__name">Cursor</span></a>
  <a class="agent-chip" data-agent="vscode-copilot" href="https://github.com/features/copilot" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-vscode"/></svg></span><span class="agent-chip__name">VS Code Copilot</span></a>
  <a class="agent-chip" data-agent="visualstudio-copilot" href="https://visualstudio.microsoft.com" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-visualstudio"/></svg></span><span class="agent-chip__name">Visual Studio Copilot</span></a>
  <a class="agent-chip" data-agent="qwen" href="https://github.com/QwenLM/qwen-code" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-qwen"/></svg></span><span class="agent-chip__name">Qwen Code</span></a>
  <a class="agent-chip" data-agent="deepseek-tui" href="https://www.deepseek.com" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-deepseek"/></svg></span><span class="agent-chip__name">DeepSeek TUI</span></a>
  <a class="agent-chip" data-agent="vibe" href="https://mistral.ai" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-mistral"/></svg></span><span class="agent-chip__name">Mistral Vibe</span></a>
  <a class="agent-chip" data-agent="zed" href="https://zed.dev" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-zed"/></svg></span><span class="agent-chip__name">Zed</span></a>
  <a class="agent-chip" data-agent="warp" href="https://www.warp.dev" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-warp"/></svg></span><span class="agent-chip__name">Warp</span></a>
  <a class="agent-chip" data-agent="amp" href="https://ampcode.com" target="_blank" rel="noopener" title="Deprecated: historical local Amp thread JSON only"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-sourcegraph"/></svg></span><span class="agent-chip__name">Amp (historical)</span></a>
  <a class="agent-chip" data-agent="opencode" href="https://opencode.ai" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-opencode"/></svg></span><span class="agent-chip__name">OpenCode</span></a>
  <a class="agent-chip" data-agent="positron" href="https://positron.posit.co" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-posit"/></svg></span><span class="agent-chip__name">Positron</span></a>
  <a class="agent-chip" data-agent="posit-assistant" href="https://github.com/posit-dev/assistant" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-posit"/></svg></span><span class="agent-chip__name">Posit Assistant</span></a>
  <a class="agent-chip" data-agent="cowork" href="https://www.anthropic.com" target="_blank" rel="noopener"><span class="agent-chip__glyph"><svg viewBox="0 0 24 24" aria-hidden="true"><use xlink:href="#i-claude"/></svg></span><span class="agent-chip__name">Claude Cowork</span></a>
  <a class="agent-chip" data-agent="aider" href="https://aider.chat" target="_blank" rel="noopener"><span class="agent-chip__glyph agent-chip__glyph--mono">Ai</span><span class="agent-chip__name">Aider</span></a>
  <a class="agent-chip" data-agent="antigravity" href="https://antigravity.google" target="_blank" rel="noopener"><span class="agent-chip__glyph agent-chip__glyph--mono">Ag</span><span class="agent-chip__name">Antigravity</span></a>
  <a class="agent-chip" data-agent="gptme" href="https://gptme.org" target="_blank" rel="noopener"><span class="agent-chip__glyph agent-chip__glyph--mono">gm</span><span class="agent-chip__name">gptme</span></a>
  <a class="agent-chip" data-agent="kilo" href="https://kilocode.ai" target="_blank" rel="noopener"><span class="agent-chip__glyph agent-chip__glyph--mono">Kl</span><span class="agent-chip__name">Kilo</span></a>
  <a class="agent-chip" data-agent="kimi" href="https://www.kimi.com" target="_blank" rel="noopener"><span class="agent-chip__glyph agent-chip__glyph--mono">Km</span><span class="agent-chip__name">Kimi</span></a>
  <a class="agent-chip" data-agent="kiro" href="https://kiro.dev" target="_blank" rel="noopener"><span class="agent-chip__glyph agent-chip__glyph--mono">Kr</span><span class="agent-chip__name">Kiro</span></a>
  <a class="agent-chip" data-agent="openhands" href="https://github.com/All-Hands-AI/OpenHands" target="_blank" rel="noopener"><span class="agent-chip__glyph agent-chip__glyph--mono">OH</span><span class="agent-chip__name">OpenHands</span></a>
  <a class="agent-chip" data-agent="zcode" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">Zc</span><span class="agent-chip__name">ZCode</span></a>
  <a class="agent-chip" data-agent="zencoder" href="https://zencoder.ai" target="_blank" rel="noopener"><span class="agent-chip__glyph agent-chip__glyph--mono">Ze</span><span class="agent-chip__name">Zencoder</span></a>
  <a class="agent-chip" data-agent="commandcode" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">Cc</span><span class="agent-chip__name">Command Code</span></a>
  <a class="agent-chip" data-agent="cortex-code" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">Cx</span><span class="agent-chip__name">Cortex Code</span></a>
  <a class="agent-chip" data-agent="forge" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">Fo</span><span class="agent-chip__name">Forge</span></a>
  <a class="agent-chip" data-agent="grok" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">Gr</span><span class="agent-chip__name">Grok</span></a>
  <a class="agent-chip" data-agent="hermes" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">He</span><span class="agent-chip__name">Hermes</span></a>
  <a class="agent-chip" data-agent="iflow" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">iF</span><span class="agent-chip__name">iFlow</span></a>
  <a class="agent-chip" data-agent="mimocode" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">Mi</span><span class="agent-chip__name">MiMoCode</span></a>
  <a class="agent-chip" data-agent="omp" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">Om</span><span class="agent-chip__name">OhMyPi</span></a>
  <a class="agent-chip" data-agent="openclaw" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">Oc</span><span class="agent-chip__name">OpenClaw</span></a>
  <a class="agent-chip" data-agent="pi" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">&#960;</span><span class="agent-chip__name">Pi</span></a>
  <a class="agent-chip" data-agent="piebald" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">Pb</span><span class="agent-chip__name">Piebald</span></a>
  <a class="agent-chip" data-agent="qclaw" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">Qc</span><span class="agent-chip__name">QClaw</span></a>
  <a class="agent-chip" data-agent="qwenpaw" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">Qp</span><span class="agent-chip__name">QwenPaw</span></a>
  <a class="agent-chip" data-agent="reasonix" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">Rx</span><span class="agent-chip__name">Reasonix</span></a>
  <a class="agent-chip" data-agent="shelley" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">Sh</span><span class="agent-chip__name">Shelley</span></a>
  <a class="agent-chip" data-agent="workbuddy" href="/configuration/#session-discovery"><span class="agent-chip__glyph agent-chip__glyph--mono">Wb</span><span class="agent-chip__name">WorkBuddy</span></a>
</div>

## Quick Start

**Download the desktop app (recommended):**

Download the latest `.dmg` (macOS), `.exe` (Windows), or `.AppImage` (Linux)
from [GitHub Releases](https://github.com/kenn-io/agentsview/releases) or via
homebrew: `brew install --cask agentsview`. The desktop app is fully bundled and
includes built-in auto-update.

**Install via pip** — or run instantly with `uvx`:

```bash
pip install agentsview    # install permanently
uvx agentsview            # or run without installing
```

**Install via shell script:**

```bash
curl -fsSL https://agentsview.io/install.sh | bash
```

**Windows (PowerShell):**

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://agentsview.io/install.ps1 | iex"
```

```bash
agentsview serve          # Start the server in the foreground
agentsview daemon start   # Start the writable SQLite daemon
agentsview daemon status  # Show daemon status
agentsview daemon restart # Restart from current configuration
agentsview daemon stop    # Stop the writable daemon
```

The existing `serve --background`, `serve status`, and `serve stop` forms also
remain available for compatibility and one-off flag-driven launches. See the
[CLI reference](/commands/#agentsview-daemon) for lifecycle details.

!!! note

    The desktop app and CLI share the same data directory (`~/.agentsview/`), so
    you can use one or both — they are fully complementary.

## Fast Token Usage & Cost Reports

If you've been reaching for [`ccusage`](https://github.com/ryoppippi/ccusage) to
see how much you spent on Claude Code yesterday, try
[`agentsview usage`](/token-usage/) instead. It reads from the same pre-indexed
SQLite database that powers the rest of AgentsView, so reports come back in well
under a second even on large histories. It reports on token-bearing sessions
from Claude Code, Codex, Copilot CLI, OpenCode-format tools, Pi, Gemini, Qwen
Code, OpenClaw/QClaw, Hermes, WorkBuddy, Forge, Piebald, Antigravity, Zed, VS
Code Copilot, Visual Studio Copilot, gptme, Mistral Vibe, and more as parser
coverage expands.

```bash
agentsview usage daily          # last 30 days, terminal table
agentsview usage daily --all    # full history, JSON-friendly
agentsview usage statusline     # $9.61 today
```

On a 22,000-session local database, `agentsview usage daily` runs **80–220×
faster** than `npx ccusage@latest daily` (see
[benchmarks](/token-usage/#how-it-compares-to-ccusage)). On smaller databases
the absolute gap is smaller, but reports still come back sub-second. See
[Token Usage & Costs](/token-usage/) for the full write-up.

## See When Your Agents Are Working

The [**Activity**](/activity/) dashboard turns timestamped session data into a
clear picture of *when* your agents ran, how much work overlapped, and what it
cost. See peak concurrency and the exact moment it happened, active versus idle
time, agent-minutes across concurrent sessions, and total cost — scoped to any
day, week, month, or custom range and filterable by project, agent, and machine.

![AgentsView Activity dashboard](/assets/generated/screenshots/activity-page.png)

Click any bucket in the concurrency timeline to see exactly which sessions were
running in that slot, overlay token or cost trends over the bars, and break
activity down by project, model, or agent. The same report is available from the
CLI, with `--json` for scripting:

```bash
agentsview activity report --preset day
agentsview activity report --preset week --json
```

See [Activity](/activity/) for the full reference.

## What It Does

AgentsView reads the session files that your
[AI coding agents](/configuration/#session-discovery) leave on your machine and
gives you a local-first desktop and web app to work with them. By default
everything stays on your machine. For multi-machine use, a primary viewer can
read [out-of-band filesystem copies](/filesystem-sync/), or
[PostgreSQL sync](/pg-sync/) can push session data to a shared database.

<div class="grid cards" markdown>

-   **AI-Powered Insights**

    Generate summaries and analysis of your coding sessions
    using Claude, Codex, Copilot, or Gemini. Get daily
    activity digests, multi-day analyses, and
    recommendations — scoped by project or across everything.

-   **Browse Sessions**

    Scroll through every session across all your projects.
    See the full conversation: user prompts, assistant
    responses, thinking blocks, and tool calls. Filter by
    project, agent, date, or message count.

-   **Search Everything**

    Full-text search across all message content. Find that
    one conversation where you discussed a specific function,
    error message, or design decision — even months later.
    Opt-in [semantic search](/semantic-search/) matches by
    meaning when you don't remember the exact words, and every
    match cites the conversation unit it came from.

-   **Recent Edits**

    A cross-session feed of the files your agents changed most
    recently, grouped by project and path. Expand any file to see
    its edits and jump straight to the message that made each
    change, on the [Recent Edits](/recent-edits/) page.

-   **Analyze Your Usage**

    Activity heatmaps, tool usage breakdowns, velocity
    metrics, session-health analytics, per-project stats, and
    session distribution charts. Understand how you use agents
    over time.

-   **Activity & Concurrency**

    See when your agents were actually working, how much ran
    in parallel, and what it cost — peak concurrency, active
    versus idle time, agent-minutes, and cost over any time
    window, on the [Activity](/activity/) page.

-   **Token Usage & Costs**

    A sub-second [`agentsview usage`](/token-usage/) CLI for
    daily spend reports and a today's-cost status line. A
    `ccusage` alternative for token-bearing sessions across
    multiple agents — including Claude Code, Codex, Copilot CLI,
    VS Code Copilot, and Zed — that runs 80–220× faster on large
    session histories.

-   **Live Sync**

    Watches your session directories for changes and
    streams new messages in real time. Start a coding
    session in one window, watch it appear in AgentsView
    in another.

-   **Multi-Agent Support**

    Works with [dozens of AI coding session sources](/configuration/#session-discovery)
    including Claude Code, OpenClaude, Codex, Copilot, Cursor,
    Gemini, OpenHands, Aider, Claude Cowork, DeepSeek TUI, gptme,
    Grok, Kilo, MiMoCode, Mistral Vibe, OhMyPi, QwenPaw, Reasonix,
    Shelley, Visual Studio Copilot, and ZCode. Auto-discovers session
    directories so there's nothing to configure.

-   **Import Chat History**

    Import your [Claude.ai and ChatGPT](/chat-import/)
    conversations — including images. Upload a zip export
    and browse everything in one place alongside your
    agent coding sessions.

-   **Runs Locally**

    SQLite database, embedded web frontend, no cloud
    services, no accounts. Install the desktop app or
    a single binary and run it.

</div>

## How It Works

<img src="/assets/static/architecture.svg" alt="AgentsView architecture: agent sessions sync into SQLite with FTS5 search, served via REST API, SSE events, and embedded Svelte SPA" style="width: 100%; max-width: 960px; margin: 1.5rem auto; display: block;" />

AgentsView watches your agent session directories for changes, parses JSONL
files from each agent format, and stores structured data in SQLite with
full-text search indexes. The embedded web frontend provides browsing, search,
and analytics over the REST API.
