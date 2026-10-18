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


class PostgresStatsTest(unittest.TestCase):
    def test_tables_and_indexes_are_read_from_the_run(self):
        with tempfile.TemporaryDirectory() as directory:
            with open(os.path.join(directory, "pg-tables.csv"), "w") as f:
                f.write(
                    "relname,seq_scan,idx_scan,n_tup_ins,n_tup_upd,n_tup_hot_upd,n_dead_tup\n"
                    "wallets,3,9000,100000,4000,3800,120\n"
                    "transactions,0,12000,104000,0,0,0\n"
                )
            with open(os.path.join(directory, "pg-indexes.csv"), "w") as f:
                f.write(
                    "relname,indexrelname,idx_scan\n"
                    "transactions,transactions_pkey,10\n"
                    "transactions,idx_transactions_reference,0\n"
                    "wallets,wallets_pkey,9000\n"
                )
            tables = summarize.pg_tables(directory)
            unused = summarize.unused_indexes(directory)
        self.assertEqual(tables["wallets"]["n_tup_hot_upd"], 3800)
        self.assertEqual(summarize.hot_percent(tables), "95")
        self.assertEqual(unused, ["idx_transactions_reference"])

    def test_missing_files_read_as_nothing(self):
        with tempfile.TemporaryDirectory() as directory:
            self.assertEqual(summarize.pg_tables(directory), {})
            self.assertEqual(summarize.unused_indexes(directory), [])
            self.assertEqual(summarize.hot_percent({}), "—")


class PerRequestCostTest(unittest.TestCase):
    def test_commits_and_postgres_cpu_per_request(self):
        series = {
            "server_rps": [(10, 500.0), (20, 500.0)],
            "pg_commits_per_second": [(10, 560.0), (20, 540.0)],
        }
        cpu = {"postgres": [(10, 150.0), (20, 150.0)], "app": [], "k6": []}
        self.assertEqual(summarize.commits_per_request(series, 10, 20), "1.10")
        self.assertEqual(summarize.pg_ms_per_request(series, cpu, 10, 20), "3.00")
        self.assertEqual(summarize.commits_per_request({}, 10, 20), "—")


if __name__ == "__main__":
    unittest.main()
