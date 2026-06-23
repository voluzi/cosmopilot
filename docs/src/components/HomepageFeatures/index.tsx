import type {ReactNode} from 'react';
import clsx from 'clsx';
import ThemedImage from '@theme/ThemedImage';
import useBaseUrl from '@docusaurus/useBaseUrl';
import Heading from '@theme/Heading';
import styles from './styles.module.css';

type FeatureItem = {
  title: string;
  image: {light: string; dark: string};
  description: ReactNode;
};

const FeatureList: FeatureItem[] = [
  {
    title: 'Upgrades',
    image: {light: '/features/upgrades.png', dark: '/features/upgrades-dark.png'},
    description: (
      <>Monitors the chain for governance upgrades and automatically upgrades the nodes without manual intervention.</>
    ),
  },
  {
    title: 'Peering',
    image: {light: '/features/peering.png', dark: '/features/peering-dark.png'},
    description: <>Automatically peers nodes from the same network if they are on the same namespace.</>,
  },
  {
    title: 'PVC Resize',
    image: {light: '/features/pvc-resize.png', dark: '/features/pvc-resize-dark.png'},
    description: <>Automatically increases PVC size when usage exceeds a configurable threshold.</>,
  },
  {
    title: 'API Endpoints Exposure',
    image: {light: '/features/api.png', dark: '/features/api-dark.png'},
    description: (
      <>Allows to publicly expose node&apos;s API endpoints with fine-grained access control and caching for increased performance.</>
    ),
  },
  {
    title: 'Volume Snapshots',
    image: {light: '/features/snapshot.png', dark: '/features/snapshot-dark.png'},
    description: (
      <>Periodically takes volume snapshots based on policies, verifies their integrity and optionally exports them as tarballs to external storage.</>
    ),
  },
  {
    title: 'Genesis Download',
    image: {light: '/features/genesis.png', dark: '/features/genesis-dark.png'},
    description: <>Retrieve genesis from a URL, ConfigMap, RPC endpoint, or generate a new one (useful for launching testnets).</>,
  },
  {
    title: 'State-sync',
    image: {light: '/features/state-sync.png', dark: '/features/state-sync-dark.png'},
    description: <>Automatically configures state-sync between nodes, simplifying node recovery when needed.</>,
  },
  {
    title: 'TmKMS Integration',
    image: {light: '/features/signing.png', dark: '/features/signing-dark.png'},
    description: <>Securely manages private keys with TmKMS (with support for HashiCorp Vault as the key provider).</>,
  },
];

function Feature({title, image, description}: FeatureItem) {
  return (
    <div className={clsx('col col--3')}>
      <div className="text--center">
        <ThemedImage
          className={styles.featureImg}
          alt={title}
          sources={{
            light: useBaseUrl(image.light),
            dark: useBaseUrl(image.dark),
          }}
        />
      </div>
      <div className="text--center padding-horiz--md">
        <Heading as="h3">{title}</Heading>
        <p>{description}</p>
      </div>
    </div>
  );
}

export default function HomepageFeatures(): ReactNode {
  return (
    <section className={styles.features}>
      <div className="container">
        <div className="row">
          {FeatureList.map((props, idx) => (
            <Feature key={idx} {...props} />
          ))}
        </div>
      </div>
    </section>
  );
}
