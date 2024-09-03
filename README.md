# WACSV - a simple golang program to send whatsapp messages using cli.

This is an adaptation of mdtest from [whatsmeow][go.mau.fi/whatsmeow/mdtest/wacsv] repo.

installation assumes you have a working golang installation. Certain paths and behaviours assume a linux system. However, it should not be difficult to adapt this program for windows usage.

installation:
    go get github.com/adityamj/wacsv
    go install  github.com/adityamj/wacsv

[go.mau.fi/whatsmeow/mdtest/wacsv]: go.mau.fi/whatsmeow/mdtest/wacsv

# Example usage:
    wacsv -db-address ./mydb.sqlite3 -ignore 0 -media <image.jpg/png> -img -template <mytemplate.txt> -sleep 500 -w 60

## Template Sample
    
    Hi {{ 2 }},
    
    Here is an exciting offer for you. Please use the coupon code {{ 3 }} for an exclusive 33% discount on your next purchase!
    

Here `2` indicates third column, index starts from 0

CSV file would look like

    mobile,email,name,offer_code
    99xxx99,abc@example.com,Example Name,EX33TODAY

