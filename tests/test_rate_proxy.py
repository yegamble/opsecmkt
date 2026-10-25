"""The optional rate-feed Tor proxy preserves application network isolation.

These checks resolve Docker Compose configuration and read the mounted torrc.
They need no Docker daemon, start no services and make no public requests.
As with the other Compose checks, missing docker compose skips this suite.
"""
import ipaddress
from pathlib import Path
import unittest
from urllib.parse import urlparse

from test_compose_exposure import compose_available, exposure_problems, resolved

REPOSITORY = Path(__file__).resolve().parents[1]
TORRC = REPOSITORY / 'deploy' / 'tor' / 'rates' / 'torrc'


@unittest.skipUnless(compose_available(), 'docker compose is not available')
class ResolvedRateProxyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.configs = {}
        for mode in ('tor', 'clearnet'):
            files = ['-f', 'compose.yaml', '-f', f'compose.{mode}.yaml',
                     '-f', 'compose.internal-db.yaml', '-f', 'compose.rate-proxy.yaml']
            cls.configs[mode] = resolved(files, ['--profile', 'internal-db'],
                                         {'APP_MODE': mode,
                                          'FIAT_RATE_PROXY_URL': 'socks5h://rate-tor:9050',
                                          'FIAT_RATE_RELAY_URL': ''})

    def test_only_rate_tor_gains_the_proxy_egress_network(self):
        for mode, config in self.configs.items():
            with self.subTest(mode=mode):
                services, networks = config['services'], config['networks']
                self.assertEqual(set(services['app']['networks']), {'backend', 'rate_proxy'})
                self.assertIs(networks['rate_proxy'].get('internal'), True)
                self.assertIsNot(networks['rate_proxy_egress'].get('internal'), True)
                if mode == 'tor':
                    self.assertIs(networks['backend'].get('internal'), True)
                members = lambda network: sorted(name for name, service in services.items()
                                                  if network in service.get('networks', {}))
                self.assertEqual(members('rate_proxy'), ['app', 'rate-tor'])
                self.assertEqual(members('rate_proxy_egress'), ['rate-tor'])
                self.assertEqual(set(services['rate-tor']['networks']),
                                 {'rate_proxy', 'rate_proxy_egress'})
                self.assertNotIn('external_egress', services['app']['networks'])

    def test_proxy_publishes_no_port_and_keeps_existing_exposure(self):
        for mode, config in self.configs.items():
            with self.subTest(mode=mode):
                self.assertEqual(exposure_problems(config, mode), [])
                proxy = config['services']['rate-tor']
                self.assertFalse(proxy.get('ports'))
                self.assertEqual(proxy['user'], 'debian-tor')
                self.assertIs(proxy.get('read_only'), True)
                self.assertEqual(proxy.get('cap_drop'), ['ALL'])
                self.assertIn('no-new-privileges:true', proxy.get('security_opt', []))

    def test_socks_binds_only_the_internal_address_and_rejects_the_gateway(self):
        options = []
        for line in TORRC.read_text().splitlines():
            line = line.split('#', 1)[0].strip()
            if line:
                options.append(tuple(line.split(None, 1)))
        for mode, config in self.configs.items():
            with self.subTest(mode=mode):
                proxy = config['services']['rate-tor']
                (ipam,) = config['networks']['rate_proxy']['ipam']['config']
                address = ipaddress.ip_address(proxy['networks']['rate_proxy']['ipv4_address'])
                gateway = ipaddress.ip_address(ipam['gateway'])
                subnet = ipaddress.ip_network(ipam['subnet'])
                dynamic = ipaddress.ip_network(ipam['ip_range'])
                self.assertIn(address, subnet)
                self.assertIn(gateway, subnet)
                self.assertNotIn(address, dynamic)
                self.assertNotIn(gateway, dynamic)
                self.assertNotEqual(address, gateway)
                endpoint = urlparse(config['services']['app']['environment']['FIAT_RATE_PROXY_URL'])
                self.assertEqual((endpoint.scheme, endpoint.hostname), ('socks5h', 'rate-tor'))
                self.assertEqual([value for key, value in options if key == 'SocksPort'],
                                 [f'{address}:{endpoint.port}'])
                self.assertEqual([value for key, value in options if key == 'SocksPolicy'],
                                 [f'reject {gateway}', f'accept {subnet}', 'reject *'])
                self.assertEqual([value for key, value in options if key == 'ControlPort'], ['0'])
                self.assertFalse(any(key.startswith('HiddenService') for key, _ in options))
                mounted = [(Path(volume['source']).relative_to(REPOSITORY).as_posix(),
                            volume['target'], volume.get('read_only'))
                           for volume in proxy['volumes'] if volume['type'] == 'bind']
                self.assertEqual(mounted, [('deploy/tor/rates/torrc', '/etc/tor/rates/torrc', True)])
                self.assertEqual(proxy['entrypoint'], ['tor', '-f', '/etc/tor/rates/torrc'])
                self.assertIn(f'/dev/tcp/{address}/{endpoint.port}',
                              ' '.join(proxy['healthcheck']['test']))
