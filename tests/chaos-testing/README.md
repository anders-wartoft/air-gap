
This a docker-based simulation testing environment aim's to simulate harsh
condition and ensure that packets can pass through air-gap without in any way
changing the contents of a message or introduce duplicates.

# Usage
Build the source by going to the root of the repository and running 
`make all`. 

Thereafter go back to the chaos-testing folder and run `docker-compose up`. If,
after a short while, you see a log in the style
```
chaos-1  | PRODUCED:  100  | RECEIVED:  100  | DUPLICATED:  0  | DROPPED:  0  | FORWARDED:  100  | UNKNOWN:  0  | EPS COUNT:  100
```
everything the test is working. If `RECEIVED` stays at 0 after two minutes, you
should first make sure the dedup is up and running. Run `docker-compose logs
dedup`, you may need to change the path to the dedup jar file in the
docker-compose.

You can play with a lot of values in the test suite. The primary ones is the
`*.properties` files to manage the go-programs, `log4j2.xml` and `dedup.env`
for the `dedup` process. 

The test-program can be configured directly in the source code, where most
relevant values are above the main function. The program also provides access
to real-time configuration through a telnet API. You can access it with `nc
localhost 1234`.

The test-program output logs for real-time updates, as well as a comprehensive
`stats.csv` file that can be used for further insights.

# Functionality
The docker-compose configures a UDP transfer setup. It contains kafka,
upstream, downstream and dedup configured to pass messages from the send topic
to receive topic. Create and resend is configured to pass the gaps data
automatically. 

The `main.go` is the main test-program file. It consist of five processes
- Producer: Produces messages into send
- Consumer: Consumes messages from dedup
- Man-in-the-Middle (MITM): Intercepts messages between upstream and downstream
  and drops messages according to a probability.
- Manager: Provides a telnet server for real-time configuration
- Stats: Logs statistics and provides stats.csv

# Tip
To reset the entire testing, run:
```
docker-compose down
docker-compose up
```

For real-time insights into memory consumption, cpu usage and more, run:
```
docker stats
```

To manage individual components, run:
```
docker-compose start/stop/rm/up/down/logs kafka
docker-compose start/stop/rm/up/down/logs downstream
docker-compose start/stop/rm/up/down/logs upstream
docker-compose start/stop/rm/up/down/logs resend
docker-compose start/stop/rm/up/down/logs create
docker-compose start/stop/rm/up/down/logs chaos
```
