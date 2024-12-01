import matplotlib.pyplot as plt
from tabulate import tabulate
from math import ceil

# -------------------------- AWS Pricing Constants ------------------- 
# Constants for AWS Lambda Pricing --> found on AWS billing console page 
AWS_LAMBDA_PRICE_PER_INVOCATION = 0.0000002  # $0.20 per 1M requests
AWS_LAMBDA_PRICE_PER_GB_SECOND = 0.00001667  # $0.01667 per GB-second

# Constants for Redis/ElastiCache --> found on AWS billing console page 
REDIS_PRICE_PER_GB_HOUR = 0.125  # $0.125 per GB-hour for Redis data storage
REDIS_PRICE_PER_ECPU = 0.0034  # $0.0034 per Redis ECPU

# API Gateway and CloudWatch --> found on AWS billing console page 
API_GATEWAY_COST_PER_REQUEST = 0.000001  # $1.00 per 1M requests
CLOUDWATCH_LOG_COST_PER_GB = 0.50  # $0.50 per GB ingested

POLLING_INTERVAL_SECONDS = 5
SESSION_DURATION_MINUTES = 10
REQUESTS_PER_SESSION = 10

# --------------- LAMBDA PARAMETERS from cloudwatch logs ---------------
POLL_LAMBDA_MEMORY_MB = 128
# bottleneck --> saw in cloudwatch proxy polls is charged 5007 ms 
POLL_LAMBDA_EXECUTION_TIME_MS = 5007 #------------- if we improve this, we improve cost of serverless 

CLIENT_LAMBDA_MEMORY_MB = 128
CLIENT_LAMBDA_EXECUTION_TIME_MS = 490

ANSWER_LAMBDA_MEMORY_MB = 128
ANSWER_LAMBDA_EXECUTION_TIME_MS = 7

SESSION_DURATION_SECONDS = SESSION_DURATION_MINUTES * 60
SESSIONS_PER_DAY = 86400 // SESSION_DURATION_SECONDS

#----------------- Server Costs ----------------------------
EC2_INSTANCE_COST_PER_HOUR = 0.08  # t3.medium, adjust as needed
STORAGE_COST_PER_GB_MONTH = 0.10  #S3 standard storage cost
NETWORK_COST_PER_GB = 0.09  # outbound data transfer cost
ALB_COST_PER_HOUR = 0.0225
ALB_COST_PER_GB = 0.008
#----------------- Server Estimations ----------------------------
BROKER_DATA_USAGE_GB_PER_CLIENT = 0.01 # Estimate might want to tweak 
BROKER_DATA_USAGE_GB_PER_PROXY = 0.01 # Estimate  might want to tweak 
BROKER_INSTANCES = 2

def redis_storage():
    num_hours_month = 730  # Approximate hours in a month
    num_GB = 0.34  # Estimated storage size in GB // 273.383 on AWS website 
    return num_hours_month * num_GB

def redis_computation(num_clients):
    total_requests_per_client = SESSIONS_PER_DAY * REQUESTS_PER_SESSION
    total_requests_per_month = num_clients * total_requests_per_client * 30  # Assuming 30 days in a month
    execution_time_per_request_seconds = 0.34 # how long it takes for each request avg 
    total_compute_time_seconds = total_requests_per_month * execution_time_per_request_seconds
    total_ecpu_hours = total_compute_time_seconds / 3600  # Convert to hours
    return total_ecpu_hours

def calculate_lambda_cost(invocations, execution_time_ms, memory_mb):
    execution_time_seconds = execution_time_ms / 1000
    memory_gb = memory_mb / 1024
    gb_seconds = execution_time_seconds * memory_gb * invocations
    cost_gb_seconds = gb_seconds * AWS_LAMBDA_PRICE_PER_GB_SECOND
    cost_invocations = invocations * AWS_LAMBDA_PRICE_PER_INVOCATION
    return cost_gb_seconds + cost_invocations


def calculate_server_cost(num_clients, num_proxies):
    compute_hours_month = 730  # num hours in a month 

    compute_cost = EC2_INSTANCE_COST_PER_HOUR * compute_hours_month * BROKER_INSTANCES

    # storage 
    storage_gb = 15 + (num_clients * 0.001)  # Base storage + per-client overhead
    storage_cost = storage_gb * STORAGE_COST_PER_GB_MONTH

    # network 
    network_gb_clients = num_clients * BROKER_DATA_USAGE_GB_PER_CLIENT * 30 
    network_gb_proxies = num_proxies * BROKER_DATA_USAGE_GB_PER_PROXY * 30  
    total_network_gb = network_gb_clients + network_gb_proxies
    network_cost = total_network_gb * NETWORK_COST_PER_GB

    # ALB
    alb_fixed_cost = ALB_COST_PER_HOUR * compute_hours_month  # Fixed hourly cost
    alb_data_cost = total_network_gb * ALB_COST_PER_GB  # Data transfer cost
    alb_cost = alb_fixed_cost + alb_data_cost

    total_server_cost = compute_cost + storage_cost + network_cost + alb_cost
    return total_server_cost


def calculate_costs_varying_proxies(num_clients, num_proxies):
    proxy_polls_per_day = num_proxies * (86400 / POLLING_INTERVAL_SECONDS)
    client_calls_per_day = num_clients * SESSIONS_PER_DAY
    answer_calls_per_day = num_clients * SESSIONS_PER_DAY

    polls_daily_cost = calculate_lambda_cost(
        invocations=proxy_polls_per_day,
        execution_time_ms=POLL_LAMBDA_EXECUTION_TIME_MS,
        memory_mb=POLL_LAMBDA_MEMORY_MB,
    )

    offers_daily_cost = calculate_lambda_cost(
        invocations=client_calls_per_day,
        execution_time_ms=CLIENT_LAMBDA_EXECUTION_TIME_MS,
        memory_mb=CLIENT_LAMBDA_MEMORY_MB,
    )

    answers_daily_cost = calculate_lambda_cost(
        invocations=answer_calls_per_day,
        execution_time_ms=ANSWER_LAMBDA_EXECUTION_TIME_MS,
        memory_mb=ANSWER_LAMBDA_MEMORY_MB,
    )

    lambda_daily_cost = polls_daily_cost + offers_daily_cost + answers_daily_cost
    lambda_monthly_cost = lambda_daily_cost * 30

    api_gateway_monthly_cost = API_GATEWAY_COST_PER_REQUEST * (
        proxy_polls_per_day + client_calls_per_day + answer_calls_per_day
    ) * 30

    cloudwatch_monthly_cost = CLOUDWATCH_LOG_COST_PER_GB * 0.084

    redis_storage_cost = REDIS_PRICE_PER_GB_HOUR * redis_storage()
    redis_processing_cost = REDIS_PRICE_PER_ECPU * redis_computation(num_clients)
    redis_total_cost = redis_storage_cost + redis_processing_cost

    total_monthly_cost = lambda_monthly_cost + redis_total_cost + api_gateway_monthly_cost + cloudwatch_monthly_cost

    return lambda_daily_cost, lambda_monthly_cost, redis_total_cost, api_gateway_monthly_cost, cloudwatch_monthly_cost, total_monthly_cost, polls_daily_cost, offers_daily_cost, answers_daily_cost

def caluculate_all_lambda_costs(num_clients, num_proxies):
    proxy_polls_per_day = num_proxies * (86400 / POLLING_INTERVAL_SECONDS)
    client_calls_per_day = num_clients * SESSIONS_PER_DAY
    answer_calls_per_day = num_clients * SESSIONS_PER_DAY

    polls_daily_cost = calculate_lambda_cost(
        invocations=proxy_polls_per_day,
        execution_time_ms=POLL_LAMBDA_EXECUTION_TIME_MS,
        memory_mb=POLL_LAMBDA_MEMORY_MB,
    )

    offers_daily_cost = calculate_lambda_cost(
        invocations=client_calls_per_day,
        execution_time_ms=CLIENT_LAMBDA_EXECUTION_TIME_MS,
        memory_mb=CLIENT_LAMBDA_MEMORY_MB,
    )

    answers_daily_cost = calculate_lambda_cost(
        invocations=answer_calls_per_day,
        execution_time_ms=ANSWER_LAMBDA_EXECUTION_TIME_MS,
        memory_mb=ANSWER_LAMBDA_MEMORY_MB,
    )
    return polls_daily_cost * 30, offers_daily_cost * 30, answers_daily_cost * 30

def calculate_lambda_costs_table(headers, proxy_ranges, client_ranges):
    results = []
    for num_clients in client_ranges:
        for num_proxies in proxy_ranges:
            polls_cost, offers_cost, answers_cost = caluculate_all_lambda_costs(num_clients, num_proxies)
            results.append([num_clients, num_proxies, f"${polls_cost:.2f}", f"${offers_cost:.2f}", f"${answers_cost:.2f}", f"${(polls_cost+offers_cost+answers_cost):.2f}"])
    results.sort(key=lambda x: float(x[-1][1:].replace(",", ""))) 
    return results

def calculate_costs(num_clients, num_proxies):
    proxy_polls_per_day = num_proxies * (86400 / POLLING_INTERVAL_SECONDS)
    client_calls_per_day = num_clients * SESSIONS_PER_DAY
    answer_calls_per_day = num_clients * SESSIONS_PER_DAY

    polls_daily_cost = calculate_lambda_cost(
        invocations=proxy_polls_per_day,
        execution_time_ms=POLL_LAMBDA_EXECUTION_TIME_MS,
        memory_mb=POLL_LAMBDA_MEMORY_MB,
    )

    offers_daily_cost = calculate_lambda_cost(
        invocations=client_calls_per_day,
        execution_time_ms=CLIENT_LAMBDA_EXECUTION_TIME_MS,
        memory_mb=CLIENT_LAMBDA_MEMORY_MB,
    )

    answers_daily_cost = calculate_lambda_cost(
        invocations=answer_calls_per_day,
        execution_time_ms=ANSWER_LAMBDA_EXECUTION_TIME_MS,
        memory_mb=ANSWER_LAMBDA_MEMORY_MB,
    )

    lambda_daily_cost = polls_daily_cost + offers_daily_cost + answers_daily_cost
    lambda_monthly_cost = lambda_daily_cost * 30

    api_gateway_monthly_cost = API_GATEWAY_COST_PER_REQUEST * (
        proxy_polls_per_day + client_calls_per_day + answer_calls_per_day
    ) * 30

    cloudwatch_monthly_cost = CLOUDWATCH_LOG_COST_PER_GB * 0.084

    redis_storage_cost = REDIS_PRICE_PER_GB_HOUR * redis_storage()
    redis_processing_cost = REDIS_PRICE_PER_ECPU * redis_computation(num_clients)
    redis_total_cost = redis_storage_cost + redis_processing_cost

    total_monthly_cost = lambda_monthly_cost + redis_total_cost + api_gateway_monthly_cost + cloudwatch_monthly_cost

    return lambda_daily_cost, lambda_monthly_cost, redis_total_cost, api_gateway_monthly_cost, cloudwatch_monthly_cost, total_monthly_cost, polls_daily_cost, offers_daily_cost, answers_daily_cost

def make_table(table_data, headers, caption, save_loc, client_no_proxy): 
    
    print(tabulate(table_data, headers=headers, tablefmt="grid"))
    plt.rcParams["font.family"] = "Times New Roman"

    fig, ax = plt.subplots(figsize=(10, 3))  
    ax.axis('off')

    table = ax.table(
        cellText=table_data,
        colLabels=headers,
        cellLoc="center",
        loc="center",
    )
    table.auto_set_font_size(False)
    table.set_fontsize(11)
    table.auto_set_column_width(col=list(range(len(headers))))

    # colors the different cells 
    for (i, j), cell in table.get_celld().items():
        if j == len(headers) - 3:  # serverless 
            cell.set_facecolor('#95b8d1')  
        if j == len(headers) - 2: # server 
            cell.set_facecolor('#b8e0d2')
        # if j == len(headers) - 1: # difference  
        #     cell.set_facecolor('#b8e0d2')
        if j == 0: 
            cell.set_text_props(weight='bold', family='Times New Roman')
            if client_no_proxy: 
                cell.set_facecolor('#ffee93') 
            else: 
                cell.set_facecolor('#FFDBBB') 
        
    
        cell.set_height(0.15)  # Adjust this value for row height
    plt.figtext(0.5, 0.08, caption, 
                ha="center", fontsize=10)

    plt.tight_layout(rect=[0, 0.12, 1, 1])

    # saves the image in the local directory as the name passed in 
    plt.savefig(save_loc, dpi=300)
    # plt.show()

def make_table_lambda(table_data, headers, caption, save_loc):
    print(tabulate(table_data, headers=headers, tablefmt="grid"))
    plt.rcParams["font.family"] = "Times New Roman"
    num_rows = len(table_data)
    fig, ax = plt.subplots(figsize=(6, 8))

    ax.axis('off')


    table = ax.table(
        cellText=table_data,
        colLabels=headers,
        cellLoc="center",
        loc="center",
    )

    table.auto_set_font_size(False)
    table.set_fontsize(11)
    table.auto_set_column_width(col=list(range(len(headers))))

    for (i, j), cell in table.get_celld().items():
        if j == 0: 
            cell.set_text_props(weight='bold', family='Times New Roman')
            cell.set_facecolor('#ffee93') 
        if j == 1: 
            cell.set_text_props(weight='bold', family='Times New Roman')
            cell.set_facecolor('#FFDBBB') 
        
        cell.set_height(0.04) 

    # cpation 
    plt.figtext(0.5, 0.05, caption, 
                ha="center", fontsize=10)
    plt.tight_layout(rect=[0.5, 0.5, 0.5, 0.5])
    plt.savefig(save_loc, dpi=300)









client_ranges = [20000, 40000, 60000, 80000, 100000] # same as the paper 
client_table_data = []
num_proxies = 100000

for num_clients in client_ranges:
    (
        lambda_daily, lambda_monthly, redis_cost, api_cost, cw_cost, total_monthly,
        polls_cost, offers_cost, answers_cost
    ) = calculate_costs(num_clients, num_proxies)
    
    server_cost = calculate_server_cost(num_clients, num_proxies)

    client_table_data.append([
        num_clients,
        f"${lambda_monthly:.2f}",
        f"${redis_cost:.2f}",
        f"${api_cost:.2f}",
        f"${cw_cost:.2f}",
        f"${total_monthly:.2f}",
        f"${server_cost:.2f}",  # Add server cost
        f"${server_cost - total_monthly:.2f}"
    ])




#Varying proxies while fixing client count
proxy_ranges = [10000, 50000, 100000, 150000, 200000] #[20000, 40000, 60000, 80000, 100000]
client_count = 60000

table_data_proxies = []

for num_proxies in proxy_ranges:
    (
        lambda_daily, lambda_monthly, redis_cost, api_cost, cw_cost, total_monthly,
        polls_cost, offers_cost, answers_cost
    ) = calculate_costs_varying_proxies(client_count, num_proxies)

    server_cost = calculate_server_cost(num_clients, num_proxies)

    table_data_proxies.append([
        num_proxies,
        f"${lambda_monthly:.2f}",
        f"${redis_cost:.2f}",
        f"${api_cost:.2f}",
        f"${cw_cost:.2f}",
        f"${total_monthly:.2f}",
        f"${server_cost:.2f}",  # Add server cost
        f"${server_cost - total_monthly:.2f}"
    ])



# ---------------------------------------- make the tables ----------------------------------------
# 1. need headers 
# 2. need caption for below the table 
# 3. need the location to save the image 
headers_client = ["# Clients", "Lambda Functions", "Redis DB", "API Gateway", "CloudWatch Logs", "Serverless Total", "Server Based Cost", "Server - Serverless" ]
caption_client = "Table 1: Cost comparison for varying client counts and 100,000 proxies for one month"
save_loc_client = "serverless_cost_client.png"
make_table(client_table_data, headers_client, caption_client, save_loc_client, True)


headers_proxy = ["# Proxies", "Lambda Functions", "Redis DB", "API Gateway", "CloudWatch Logs", "Serverless Total", "Server Based Cost", "Server - Serverless"]
caption_proxy = "Table 2: Cost comparison for varying proxy counts and 60,000 clients for one month"
save_loc_proxy = "serverless_cost_proxy.png"
make_table(table_data_proxies, headers_proxy, caption_proxy, save_loc_proxy, False)


# make the table for varying clients and proxies 
lambda_headers = ["# Clients", "# Proxies",  "ProxyPoll", "CLientOffer" , "ProxyAnswer", "Total Cost"]
proxy_ranges = [10000, 50000, 100000, 150000, 200000]
client_ranges = [20000, 40000, 60000, 80000, 100000]
lambda_breakdown = calculate_lambda_costs_table(lambda_headers, proxy_ranges, client_ranges)
caption_lambda = "Table 3: Cost comparison of Lambda Functions\nVarying proxy and client counts for one month"
print(tabulate(lambda_breakdown, headers=lambda_headers, tablefmt="grid"))
make_table_lambda(lambda_breakdown,lambda_headers,caption_lambda, "lambda_breakdown.png")



