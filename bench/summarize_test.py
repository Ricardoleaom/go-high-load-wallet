import os
import tempfile
import unittest

import summarize


class ContainerCPUTest(unittest.TestCase):
    def test_roles_match_only_their_own_container(self):
        with tempfile.TemporaryDirectory() as directory:
            with open(os.path.join(directory, "docker-stats.csv"), "w") as f:
                f.write(
                    "100,wallet-bench-postgres-1,190.00%,1GiB / 8GiB\n"
                    "100,wallet-bench-postgres-exporter-1,0.50%,10MiB / 8GiB\n"
                    "100,wallet-bench-app-1,15.00%,50MiB / 8GiB\n"
                    "100,wallet-bench-k6-run-2f724e83f1f0,7.00%,200MiB / 8GiB\n"
                    "100,infra-database-1,0.10%,30MiB / 8GiB\n"
                )
            samples = summarize.container_cpu(directory)
        self.assertEqual(samples["postgres"], [(100, 190.0)])
        self.assertEqual(samples["app"], [(100, 15.0)])
        self.assertEqual(samples["k6"], [(100, 7.0)])


if __name__ == "__main__":
    unittest.main()
