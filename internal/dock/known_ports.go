package dock

// knownRouteProtocol classifies declared ports for automatic routing. Unknown
// ports stay in the inventory and require an explicit protocol in dock.yml.
func knownRouteProtocol(port Port) string {
	if port.Protocol != "tcp" {
		return ""
	}
	switch port.PrivatePort {
	// MySQL/MariaDB (classic and X Protocol), PostgreSQL and MongoDB.
	case 3306, 33060, 5432, 27017, 27018, 27019:
		return "tcp"
	// SQL Server, Oracle, Cassandra, Memcached, Redis and DB2.
	case 1433, 1521, 9042, 11211, 6379, 50000:
		return "tcp"
	// Mail test servers, MQTT and AMQP.
	case 1025, 1110, 1883, 5672:
		return "tcp"
	// Development servers, including Vite dev (5173) and preview (4173).
	case 3000, 3001, 4173, 4200, 5000, 5173:
		return "http"
	// Web servers, admin interfaces and HTTP APIs.
	case 80, 2019, 8000, 8001, 8025, 8080, 8081, 8082, 9000, 9001, 9090:
		return "http"
	default:
		return ""
	}
}
