import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

const config: Config = {
  title: 'Cosmopilot',
  tagline: 'Kubernetes operator for Cosmos-based blockchain nodes',
  favicon: 'favicon.ico',

  future: {
    v4: true,
  },

  url: 'https://cosmopilot.voluzi.com',
  baseUrl: '/',
  trailingSlash: false,

  onBrokenLinks: 'throw',
  onBrokenAnchors: 'warn',

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      {
        docs: {
          routeBasePath: '/',
          sidebarPath: './sidebars.ts',
          editUrl: 'https://github.com/voluzi/cosmopilot/tree/main/docs/',
          lastVersion: '2.3.0',
          versions: {
            current: {
              label: 'Next',
              path: 'next',
            },
          },
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    image: 'logo.png',
    colorMode: {
      respectPrefersColorScheme: true,
    },
    navbar: {
      title: 'Cosmopilot',
      logo: {
        alt: 'Cosmopilot',
        src: 'logo.png',
        srcDark: 'logo-dark.png',
      },
      items: [
        {type: 'doc', docId: 'getting-started/prerequisites', label: 'Getting Started', position: 'left'},
        {type: 'doc', docId: 'getting-started/chain-compatibility', label: 'Chain Compatibility', position: 'left'},
        {type: 'doc', docId: 'usage/deploy-node', label: 'Usage', position: 'left'},
        {
          type: 'docsVersionDropdown',
          position: 'right',
          dropdownActiveItemDisabled: true,
        },
        {
          href: 'https://github.com/voluzi/cosmopilot',
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Docs',
          items: [
            {label: 'Getting Started', to: '/getting-started/prerequisites'},
            {label: 'Usage', to: '/usage/deploy-node'},
            {label: 'Architecture', to: '/reference/architecture'},
            {label: 'CRDs Reference', to: '/reference/crds'},
            {label: 'Troubleshooting', to: '/operations/troubleshooting'},
          ],
        },
        {
          title: 'Examples',
          items: [
            {label: 'Cosmos Hub', to: '/examples/cosmoshub/mainnet-fullnode'},
            {label: 'Osmosis', to: '/examples/osmosis/mainnet-fullnode'},
            {label: 'Nibiru', to: '/examples/nibiru/mainnet-fullnode'},
          ],
        },
        {
          title: 'More',
          items: [
            {label: 'GitHub', href: 'https://github.com/voluzi/cosmopilot'},
            {label: 'License (MIT)', href: 'https://github.com/voluzi/cosmopilot/blob/main/LICENSE.md'},
          ],
        },
      ],
      copyright: `Released under the MIT License.<br/><a class="footer__powered-by" href="https://voluzi.com" target="_blank" rel="noopener noreferrer"><span>powered by</span><img src="/voluzi-logo.png" alt="Voluzi" /></a>`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: ['yaml', 'bash', 'go', 'json'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
